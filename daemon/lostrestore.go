package daemon

import (
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The Lost-session restore loop (#1108 PR 2): the general form of the
// root-agent self-heal. Every daemon poll, each local session marked Lost —
// its tmux vanished with no kill intent on record (outage, OOM, reboot; see
// #1104/#1122) — gets a best-effort Instance.Recover, re-spawning the program
// in its worktree. Root keeps its stronger always-ensure semantics
// (reap-and-recreate in EnsureRootAgents); everyone else is restored in place
// here. The retry discipline is #1128's verbatim: exponential backoff settling
// at the cap, never a permanent give-up, one ERROR escalation when the cause
// looks persistent. The user's off-ramp from a permanently failing restore is
// killing the session (which tombstones it).

// lostRestoreEscalationThreshold mirrors rootEnsureEscalationThreshold: the
// consecutive-failure count at which one ERROR log marks the cause as
// persistent-looking (deleted worktree, unresolvable program). The loop keeps
// retrying at the cap cadence regardless.
const lostRestoreEscalationThreshold = 6

// Backoff between failed restore attempts for one session. Package vars so
// tests can shorten them (same pattern as rootEnsureBackoff*).
var (
	lostRestoreBackoffBase = 10 * time.Second
	lostRestoreBackoffMax  = 5 * time.Minute
)

// lostRestoreState is the per-session retry state. Guarded by Manager.mu (the
// loop runs on the daemon poll goroutine; tests drive RestoreLostSessions
// directly).
type lostRestoreState struct {
	consecutiveFailures int
	nextAttempt         time.Time
	// remoteLogged dedupes the "not restoring a remote session" note to once
	// per Lost episode.
	remoteLogged bool
}

// RestoreLostSessions runs one restore pass over every Lost session the
// manager owns. Called from the daemon poll loop after EnsureRootAgents (which
// owns the reserved root title); a no-op until the initial restore finishes.
func (m *Manager) RestoreLostSessions() {
	if !m.Ready() {
		return
	}

	type entry struct {
		key      string
		repoID   string
		instance *session.Instance
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{key: key, repoID: repoID, instance: inst})
	}
	// Drop retry state for sessions that are gone or no longer Lost (healed,
	// killed, or replaced) so the map never grows unbounded.
	for key, inst := range m.instances {
		if st := m.lostRestoreStates[key]; st != nil && inst.GetStatus() != session.Lost {
			delete(m.lostRestoreStates, key)
		}
	}
	for key := range m.lostRestoreStates {
		if _, live := m.instances[key]; !live {
			delete(m.lostRestoreStates, key)
		}
	}
	m.mu.Unlock()

	// Stable order so multi-session recovery after an outage is deterministic
	// and the logs read coherently (same rationale as EnsureRootAgents).
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	for _, e := range entries {
		m.restoreLostSession(e.key, e.repoID, e.instance)
	}
}

// restoreLostSession attempts recovery of one session when it is eligible:
// Lost, local, not tombstoned, not the reserved root (EnsureRootAgents owns
// that), and no kill in flight. Recover runs under the per-session operation
// lock KillSession takes, so a kill arriving mid-attempt waits for the
// attempt and then tears down — the two operations never interleave. This
// side only TryLocks: the poll goroutine must never stall behind a slow
// teardown, and the next tick retries.
func (m *Manager) restoreLostSession(key, repoID string, inst *session.Instance) {
	if inst == nil || !inst.Started() || inst.GetStatus() != session.Lost {
		return
	}
	if inst.UserKilled() || session.IsReservedTitle(inst.Title) {
		return
	}

	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return
	}
	st := m.lostRestoreStates[key]
	if st == nil {
		st = &lostRestoreState{}
		m.lostRestoreStates[key] = st
	}
	skip := time.Now().Before(st.nextAttempt)
	m.mu.Unlock()
	if skip {
		return
	}

	if inst.IsRemote() {
		m.mu.Lock()
		logIt := !st.remoteLogged
		st.remoteLogged = true
		m.mu.Unlock()
		if logIt {
			log.InfoLog.Printf("session %q is Lost but remote; not auto-restoring (reconnect is not supported) — kill it to clear the row", inst.Title)
		}
		return
	}

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		// A kill (or its finish pass) holds the session; skip this tick.
		return
	}
	defer opLock.Unlock()

	// Re-verify under the lock: everything checked above was point-in-time,
	// and a KillSession that beat us to the lock may have torn the session
	// down (map entry gone / replaced), tombstoned it, or a racing kill may
	// have registered its intent after our killsInFlight read. Recover only
	// what is still provably a wanted Lost session.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != inst || inst.UserKilled() || inst.GetStatus() != session.Lost {
		return
	}

	if err := inst.Recover(); err != nil {
		m.lostRestoreFailed(key, st, inst.Title, err)
		return
	}

	log.InfoLog.Printf("restored lost session %q (repo %s): tmux re-spawned in its worktree", inst.Title, repoID)
	m.mu.Lock()
	delete(m.lostRestoreStates, key)
	m.mu.Unlock()
}

// lostRestoreFailed records a failed restore attempt: exponential backoff to
// lostRestoreBackoffMax where the cadence settles for as long as the failure
// persists — never a permanent give-up (#1128: an outage is indistinguishable
// from a broken worktree while it lasts; only a later retry can tell). One
// ERROR at the escalation threshold makes a persistent cause visible.
func (m *Manager) lostRestoreFailed(key string, st *lostRestoreState, title string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.consecutiveFailures++
	backoff := lostRestoreBackoffMax
	// Guard the shift: past ~16 doublings the exponential form has no meaning
	// and would overflow.
	if shift := st.consecutiveFailures - 1; shift < 16 {
		if b := lostRestoreBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	st.nextAttempt = time.Now().Add(backoff)
	if st.consecutiveFailures == lostRestoreEscalationThreshold {
		log.ErrorLog.Printf("restore of lost session %q failed %d consecutive times; the cause looks persistent — will keep retrying every %s (kill the session to stop): %v", title, st.consecutiveFailures, lostRestoreBackoffMax, err)
		return
	}
	log.WarningLog.Printf("restore of lost session %q failed (attempt %d), retrying in %s: %v", title, st.consecutiveFailures, backoff, err)
}
