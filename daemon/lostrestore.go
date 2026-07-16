package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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
//
// REMOTE sessions are not "restored in place" and never were: docker/ssh/hook
// advertise Recover, but theirs is recoverSandbox — provision a NEW sandbox and
// clone the branch back from origin, so only PUSHED state survives. That makes
// recovering a remote session destructive if it was not really lost, which is
// the asymmetry #1794 addresses: the poll debounces the Lost mark, and this loop
// re-probes the sandbox before acting on it.

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
	// vanishedWorktreesLogged dedupes the high-visibility #1303 diagnostic to
	// one ERROR per distinct missing worktree path during a Lost episode.
	vanishedWorktreesLogged map[string]struct{}
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

// lostSessionWantsRestore reports whether a session is one this loop wants back:
// Lost, started, not tombstoned, not the reserved root. It is the loop's own
// entry gate, factored out because the watch-task concurrency cap (#1892) has to
// ask the same question — see canAutoRestoreLostSession.
//
// The Recover capability is deliberately NOT part of this: restoreLostSession
// checks it separately, after the retry state exists, so it can log the
// "not auto-restoring a remote session" note once per episode rather than every
// tick. canAutoRestoreLostSession composes the two for callers that just want
// the verdict.
//
// Only a Lost session is recovery-eligible. This gate is also what fences out an
// Archived session (#1028): Archived != Lost, and an archived instance
// additionally loads with started=false, so the !Started() check short-circuits
// first — the restore loop never moves its worktree back or re-spawns its tmux.
// Restoring an archive is an explicit user action only. A UserKilled record
// means "finish this kill", never "restore this".
func lostSessionWantsRestore(inst *session.Instance) bool {
	if inst == nil || !inst.Started() || inst.GetStatus() != session.Lost {
		return false
	}
	return !inst.UserKilled() && !session.IsReservedTitle(inst.Title)
}

// canAutoRestoreLostSession reports whether RestoreLostSessions will keep trying
// to revive this session — i.e. whether restoration is still possible at all.
//
// This exists for the watch-task concurrency cap (#1892), which must keep
// counting a Lost session against max_concurrent_runs for exactly as long as this
// loop can bring it back Running. The cap cannot ask session.ClassifyActivity,
// which calls Lost TERMINAL: that verdict is right for `af sessions watch` (tell
// the user their session is lost and exit rather than poll forever) and wrong for
// the cap, because freeing the slot of a session this loop is actively retrying
// lets a capped watcher admit replacements — and when the retries then land, the
// task runs over its cap.
//
// It lives here, next to the loop whose behavior it describes, so there is ONE
// definition of restore eligibility. A copy in the cap's file would be a second
// one, free to drift the moment the gates below change.
//
// The answer is stable in the direction that matters: this loop never gives up on
// a recoverable session (#1128 — an outage is indistinguishable from a broken
// worktree while it lasts), so a slot held on this verdict is held until the
// session is restored, killed, or archived. That is the same off-ramp the loop
// itself documents. A backend that cannot be revived in place returns false
// instead: it is logged as "kill it to clear the row" and never retried, so
// restoration is genuinely not possible and the slot frees.
func canAutoRestoreLostSession(inst *session.Instance) bool {
	return lostSessionWantsRestore(inst) && inst.Capabilities().Recover
}

// restoreLostSession attempts recovery of one session when it is eligible:
// Lost, Recover-capable, not tombstoned, not the reserved root
// (EnsureRootAgents owns that), and no kill in flight. Recover runs under the
// per-session operation lock KillSession takes, so a kill arriving mid-attempt
// waits for the attempt and then tears down — the two operations never
// interleave. This side only TryLocks: the poll goroutine must never stall
// behind a slow teardown, and the next tick retries.
//
// "Recover-capable", NOT "local": docker/ssh/hook all advertise Recover, so
// remote sessions DO flow through here — and their Recover re-provisions rather
// than reconnects. That is why this function re-probes a remote sandbox before
// recovering it (#1794); see the gate below.
func (m *Manager) restoreLostSession(key, repoID string, inst *session.Instance) {
	if !lostSessionWantsRestore(inst) {
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

	if !inst.Capabilities().Recover {
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

	// Last gate before an IRREVERSIBLE step (#1794). A remote session's Recover
	// is not a reconnect — recoverSandbox provisions a BRAND-NEW sandbox and
	// clones the branch back from origin, so running it against a sandbox that
	// is in fact still up orphans that sandbox and abandons everything it never
	// pushed. The poll's debounce makes a blip-induced Lost unlikely, but this
	// row may have gone Lost many ticks ago (restore is backoff-throttled out to
	// 5 minutes) and the transport may have healed since. Re-probe NOW, against
	// live state, rather than trusting a stale verdict: an agent-server that
	// answers proves the sandbox is there, so heal the row and let the next poll
	// settle its real liveness. Bounded, so a genuinely wedged remote cannot
	// stall the poll loop here either.
	if m.remoteSandboxAnswersAlive(inst) {
		log.InfoLog.Printf("not re-provisioning lost remote session %q: its sandbox answers as alive (re-provisioning would orphan it and lose unpushed work) — clearing the Lost mark", inst.Title)
		_ = inst.Transition(session.ObserveLiveness(session.LiveRunning))
		// Clear before the persist, same ordering rule as the recovery path below:
		// the answered probe already ended the loss episode, and leaving the stale
		// count in place across a disk write lets a blip in that window re-mark the
		// very session we just proved alive.
		m.clearRemoteLoss(remoteLossKey(repoID, inst))
		m.persistInstance(repoID, inst)
		m.mu.Lock()
		delete(m.lostRestoreStates, key)
		m.mu.Unlock()
		return
	}

	if err := inst.Recover(); err != nil {
		// Persist the instance even on failure, matching the manual restore path
		// (restore.go): Recover can mutate durable worktree state before it fails
		// — a rebuild reconstructs the worktree+branch and flips branchCreatedByUs
		// true, then a later tmux spawn fails (#1532). Without this write the flag
		// is lost, so after a daemon restart the instance reloads with a stale
		// branchCreatedByUs=false; the next recovery skips the rebuild (the worktree
		// now exists) and the flag stays wrong, so kill never deletes the branch and
		// it is orphaned. persistInstance is best-effort (logs on write failure).
		m.persistInstance(repoID, inst)
		m.logVanishedWorktreeOnce(key, repoID, st, inst, err)
		m.lostRestoreFailed(key, st, inst.Title, err)
		return
	}

	// Drop the remote-loss debounce FIRST — before the log and the retry-state
	// bookkeeping below (#1794). Recovery REPLACED the runtime those failures were
	// about: for a remote session the sandbox behind them no longer exists, so the
	// accumulated count describes a thing that is gone. Left behind, it stays
	// threshold-satisfying, and the first transport blip against the FRESH sandbox
	// would immediately re-satisfy the debounce and re-provision again, orphaning
	// the sandbox we just built — the debounce defeated at exactly the moment it
	// matters most. The poll goroutine takes neither this op-lock nor
	// killsInFlight, and Recover has already cleared the restore fence, so it can
	// probe the new sandbox the instant Recover returns; every statement between
	// the swap and this reset widens that window for nothing.
	m.noteRuntimeReplaced(repoID, inst)
	// Then persist, same as the manual restore path (restore.go): Recover mutates
	// durable worktree state on the way to SUCCESS too, not only before a failure
	// — a fresh rebuild from the recorded base recreates the branch and flips
	// branchCreatedByUs, and the orphaned-branch consequence is the one spelled
	// out on the failure path above (#1841). The poll loop does NOT cover this:
	// Recover's ConfirmLive already left the instance LiveRunning, so the next
	// tick compares LiveRunning against LiveRunning and persistPollChange skips
	// the write for as long as the agent stays busy. Ordering is load-bearing —
	// this write goes AFTER noteRuntimeReplaced, never before, since a disk write
	// is exactly the kind of statement the rule above keeps out of that window.
	m.persistInstance(repoID, inst)
	log.InfoLog.Printf("restored lost session %q (repo %s): agent re-spawned in its workspace", inst.Title, repoID)
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
		if path, ok := missingWorktreePath(err); ok && st.vanishedWorktreeWasLogged(path) {
			log.WarningLog.Printf("restore of lost session %q failed %d consecutive times; missing worktree %q was already logged at ERROR — will keep retrying every %s (kill the session to stop): %v", title, st.consecutiveFailures, path, lostRestoreBackoffMax, err)
			return
		}
		log.ErrorLog.Printf("restore of lost session %q failed %d consecutive times; the cause looks persistent — will keep retrying every %s (kill the session to stop): %v", title, st.consecutiveFailures, lostRestoreBackoffMax, err)
		return
	}
	log.WarningLog.Printf("restore of lost session %q failed (attempt %d), retrying in %s: %v", title, st.consecutiveFailures, backoff, err)
}

func (m *Manager) logVanishedWorktreeOnce(key, repoID string, st *lostRestoreState, inst *session.Instance, restoreErr error) {
	worktreePath, ok := missingWorktreePath(restoreErr)
	if !ok {
		return
	}

	m.mu.Lock()
	alreadyLogged := st.vanishedWorktreeWasLogged(worktreePath)
	if !alreadyLogged {
		if st.vanishedWorktreesLogged == nil {
			st.vanishedWorktreesLogged = make(map[string]struct{})
		}
		st.vanishedWorktreesLogged[worktreePath] = struct{}{}
	}
	_, killInFlight := m.killsInFlight[key]
	m.mu.Unlock()
	if alreadyLogged {
		return
	}

	gw, _ := inst.GetGitWorktree()
	diag := sessiongit.MissingWorktreeDiagnosis{
		WorktreePath: worktreePath,
		ParentPath:   filepath.Dir(worktreePath),
	}
	if gw != nil {
		diag = gw.DiagnoseMissingWorktree()
	}
	branch := inst.GetBranch()
	if branch == "" {
		branch = diag.BranchName
	}
	liveness := inst.GetLiveness()
	op := inst.GetInFlightOp()
	userKilled := inst.UserKilled()
	teardownIntent := userKilled || killInFlight || op == session.OpKilling || op == session.OpArchiving
	classification := classifyMissingWorktree(diag.WorktreeRegistrationKnown, diag.WorktreeRegistered, teardownIntent)

	log.ErrorLog.Printf(
		"WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_id=%q repo_path=%q worktree_path=%q branch=%q liveness=%q status=%q started=%t user_killed=%t kill_in_flight=%t in_flight_op=%d external_worktree=%t branch_created_by_us=%t observed_at=%q parent_path=%q parent_exists=%t parent_stat_error=%q repo_exists=%t repo_stat_error=%q git_worktree_registered=%q git_worktree_list_error=%q branch_exists=%q branch_probe_error=%q recover_error=%q",
		classification,
		inst.Title,
		inst.ID,
		repoID,
		diag.RepoPath,
		worktreePath,
		branch,
		livenessLabel(liveness),
		statusLabel(inst.GetStatus()),
		inst.Started(),
		userKilled,
		killInFlight,
		op,
		diag.ExternalWorktree,
		diag.BranchCreatedByUs,
		time.Now().UTC().Format(time.RFC3339Nano),
		diag.ParentPath,
		diag.ParentExists,
		diag.ParentStatError,
		diag.RepoExists,
		diag.RepoStatError,
		triBool(diag.WorktreeRegistrationKnown, diag.WorktreeRegistered),
		diag.WorktreeListError,
		triBool(diag.BranchKnown, diag.BranchExists),
		diag.BranchError,
		restoreErr.Error(),
	)
}

func missingWorktreePath(err error) (string, bool) {
	var wtErr *session.WorktreeUnavailableError
	if !errors.As(err, &wtErr) || !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if wtErr.WorktreePath == "" {
		return "<empty>", true
	}
	return wtErr.WorktreePath, true
}

func (st *lostRestoreState) vanishedWorktreeWasLogged(path string) bool {
	if st == nil || st.vanishedWorktreesLogged == nil {
		return false
	}
	_, ok := st.vanishedWorktreesLogged[path]
	return ok
}

func classifyMissingWorktree(registrationKnown, registered, teardownIntent bool) string {
	if teardownIntent {
		return "expected_teardown"
	}
	if registrationKnown && registered {
		return "unexpected_external_removal"
	}
	if registrationKnown {
		return "missing_unregistered_live_worktree"
	}
	return "missing_worktree_registration_unknown"
}

func triBool(known, value bool) string {
	if !known {
		return "unknown"
	}
	return strconv.FormatBool(value)
}

func livenessLabel(lv session.Liveness) string {
	switch lv {
	case session.LivenessUnset:
		return "LivenessUnset"
	case session.LiveRunning:
		return "LiveRunning"
	case session.LiveReady:
		return "LiveReady"
	case session.LiveLost:
		return "LiveLost"
	case session.LiveDead:
		return "LiveDead"
	case session.LiveArchived:
		return "LiveArchived"
	case session.LiveLimitReached:
		return "LiveLimitReached"
	default:
		return "Liveness(" + strconv.Itoa(int(lv)) + ")"
	}
}

func statusLabel(s session.Status) string {
	switch s {
	case session.Running:
		return "Running"
	case session.Ready:
		return "Ready"
	case session.Loading:
		return "Loading"
	case session.Deleting:
		return "Deleting"
	case session.Dead:
		return "Dead"
	case session.Lost:
		return "Lost"
	case session.Archived:
		return "Archived"
	default:
		return "Status(" + strconv.Itoa(int(s)) + ")"
	}
}
