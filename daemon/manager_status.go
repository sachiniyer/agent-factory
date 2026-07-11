package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// statusPollLease bounds how long a single PauseStatusPoll silences an
// instance's daemon capture-pane liveness poll (#1160). The attached TUI
// renews it with a heartbeat every statusPollRenewInterval (< this lease) and
// clears it on clean detach, but this fixed SERVER-SIDE lease — never a
// client-supplied duration — is the leak-safety guarantee: a crashed TUI that
// never renews or resumes auto-resumes within one lease, so real tmux death is
// still detected on the next tick and the daemon can never be permanently
// blinded. var, not const, so tests can shrink it.
var statusPollLease = 3 * time.Second

// nowFunc is the clock used by the pause-poll lease logic (#1160), injectable
// so lease-expiry tests advance time deterministically instead of racing real
// sleeps.
var nowFunc = time.Now

// RefreshStatuses recomputes every started instance's status the way the TUI
// metadata tick used to (#935) and persists each transition through the
// targeted single-writer path. With the daemon the sole owner of session state
// (#960 PR 4/5), status is authoritative HERE and projected to the TUI via
// Snapshot — the TUI no longer computes it. Called once per poll from RunDaemon,
// alongside the AutoYes pass it now subsumes.
//
// The instance list is snapshotted under m.mu, then each instance's (possibly
// slow) tmux probes run with the lock released so a hung capture-pane can't
// block unrelated manager RPCs.
// PauseStatusPoll pauses the daemon's capture-pane liveness poll for one
// attached session for statusPollLease from now (#1160). Renewing (the TUI's
// heartbeat) just pushes the expiry out; the pause is per-instance, so every
// other session keeps refreshing during the attach.
func (m *Manager) PauseStatusPoll(repoID, title string) {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	m.pausedPolls[key] = nowFunc().Add(statusPollLease)
	m.pausedMu.Unlock()
}

// ResumeStatusPoll clears a pause immediately on a clean detach so the poll
// resumes on the next tick rather than waiting out the lease (#1160).
func (m *Manager) ResumeStatusPoll(repoID, title string) {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	delete(m.pausedPolls, key)
	m.pausedMu.Unlock()
}

// isPollPaused reports whether an instance's poll is currently paused (#1160).
// A present-but-expired lease is lazily deleted and reported unpaused, so a
// crashed TUI that never sent Resume auto-resumes within one lease — the
// crash-safety property that keeps a pause from ever permanently blinding the
// daemon.
func (m *Manager) isPollPaused(repoID, title string) bool {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	expiry, ok := m.pausedPolls[key]
	if !ok {
		return false
	}
	if nowFunc().Before(expiry) {
		return true
	}
	delete(m.pausedPolls, key) // lease lapsed — lazy GC, then poll as normal
	return false
}

func (m *Manager) RefreshStatuses() {
	type entry struct {
		repoID   string
		instance *session.Instance
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{repoID: repoID, instance: inst})
	}
	m.mu.Unlock()

	for _, e := range entries {
		m.refreshInstanceStatus(e.repoID, e.instance)
	}
}

// refreshInstanceStatus mirrors the old runMetadataTick body for one instance:
//   - skip unstarted instances and any with an in-flight op (an archive/restore
//     mid-teardown, a create/kill overlay — probing or writing either would poke
//     a session whose tmux is being spun up or torn down, #844/#1195);
//   - dismiss a pending trust prompt (CheckAndHandleTrustPrompt), moved here from
//     the TUI so it works whether or not a TUI is attached;
//   - HasUpdated → Running; a waiting prompt → TapEnter (the AutoYes path, which
//     this poll already owned — unchanged by #960);
//   - otherwise probe liveness: a vanished tmux/remote session → Lost (never
//     repainted Ready, the #935 invariant the hollow status-dot rendering
//     relies on; Lost rather than Dead since #1108 — no kill intent on record
//     means the session is recovery-eligible), a live idle one → Ready;
//   - a session carrying the kill-intent tombstone (#1108) short-circuits all
//     of the above: its interrupted teardown is finished instead.
//
// The poll writes only the liveness axis (SetLiveness), gated on there being no
// in-flight op — so it can never clobber a concurrent kill/archive marker, which
// lives on the separate op axis (#1195). Only a real transition is persisted, and it persists
// under the per-repo start lock (mirroring CreateTab/CloseTab/SetPRInfo) through
// the targeted writer persistInstanceData — never a whole-list re-marshal, the
// dual-writer clobber surface #960 PR 4 retired — so an idle session never churns
// instances.json.
func (m *Manager) refreshInstanceStatus(repoID string, instance *session.Instance) {
	if instance == nil || !instance.Started() {
		return
	}
	if instance.UserKilled() {
		// A surviving kill-intent tombstone (#1108) means a previous
		// KillSession was interrupted after committing to the kill. The only
		// valid future for this session is finishing that teardown — never
		// probing it, never marking it Lost, never restoring it.
		m.finishUserKill(repoID, instance)
		return
	}
	if instance.GetInFlightOp() != session.OpNone {
		// An op is mid-flight (archive/restore teardown, create/kill overlay): the
		// poll must not probe a session whose tmux is being spun up/torn down and
		// mark it Lost — the op's executor writes the settled liveness. Replaces
		// the old Loading/Deleting skip (#1195).
		return
	}
	if instance.GetLiveness() == session.LiveArchived {
		// Archived (#1028): no tmux to probe, inert (started=false) so already
		// skipped by !Started above — belt-and-suspenders against a future change.
		return
	}
	if m.isPollPaused(repoID, instance.Title) {
		// A TUI is attached full-screen to this instance (#1160). It owns the
		// shared tmux server for the attach duration; the daemon's capture-pane
		// liveness probe here would needlessly contend with the live attach and
		// hurt input responsiveness (Fix A follow-up to #1157). Skip the probe.
		// The status is left UNCHANGED — a paused instance is known-attached-and-
		// alive, never marked Lost (#1108): it has not vanished. Leak-safe: the
		// pause is lease-bounded (statusPollLease), so a crashed TUI that never
		// sends Resume auto-resumes within one lease and real death is detected
		// on the next tick — the pause can never permanently blind the daemon.
		return
	}

	// The daemon observes and drives the session ONLY through its agent-server
	// (#1592 Phase 2 PR4) — never the tmux-shaped Backend probes directly, so this
	// poll makes no assumption that the session is local tmux. For the local
	// runtime the agent-server drives tmux in-process.
	as := instance.AgentServer()
	before := instance.GetLiveness()
	beforeReset, _ := instance.LimitResetAt()
	// Snapshot dismisses a pending trust prompt then reads the pane in one probe
	// (the exact order the poll used to run CheckAndHandleTrustPrompt then
	// HasUpdated). Content is the capture handed back so the idle branch runs the
	// usage-limit detector (#1146) without a second capture-pane.
	obs, err := as.Snapshot()
	if err != nil {
		// A future remote runtime's observation channel failed: leave the status
		// for the next tick rather than misreading it. The local agent-server
		// never errors here, so this is inert today — it mirrors the paused /
		// in-flight-op early returns above (never mark Lost on a failed probe).
		return
	}
	updated, hasPrompt, content := obs.Updated, obs.HasPrompt, obs.Content
	if hasPrompt {
		// Tap enter whenever a prompt is waiting (TapEnter is a no-op unless
		// AutoYes is on), independent of `updated` — exactly as the pre-#965
		// AutoYes loop did with `if _, hasPrompt := ...Snapshot(); …`. A prompt's
		// text is itself fresh output, so a just-appeared prompt commonly reports
		// (updated, hasPrompt) == (true, true); folding the tap into the switch
		// below `case updated` swallowed it on that first tick and only tapped on
		// the next poll — a one-interval AutoYes delay (#992).
		as.TapEnter()
	}
	switch {
	case updated:
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
	case hasPrompt:
		// A waiting prompt with otherwise-unchanged output: leave the status for
		// the next tick to resolve, exactly as runMetadataTick did. The
		// prompt-tap already fired above regardless of `updated`.
	case !as.Alive():
		// Snapshot returned (false,false), which a healthy idle session and a
		// dead one both produce — indistinguishable on their own. Probe liveness
		// only on this idle branch so a vanished session is marked Lost and
		// rendered distinctly rather than repainted as a green Ready dot it can
		// no longer back (#935). Lost, not Dead (#1108): there is no kill
		// intent on record, so the session vanished out from under a live
		// record — an outage/reboot casualty that is recovery-eligible, not a
		// corpse the user wanted gone.
		_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
	default:
		// Idle output: settle to Ready, or LimitReached when the pane shows a
		// usage-limit banner for a claude/codex session (#1146). content is
		// HasUpdated's capture (no re-capture); see resolveIdleLiveness.
		m.resolveIdleLiveness(instance, content)
	}

	// Persist a liveness OR usage-limit reset-time change (#1146); see limit.go.
	m.persistPollChange(repoID, instance, before, beforeReset)
}

// SaveInstances writes the manager's authoritative in-memory instances to disk
// as a straight per-repo marshal (#960 PR 4). With the daemon the sole writer of
// instances.json there is no competing snapshot to reconcile, so this is no
// longer a merge. Every mutation already persists through a targeted writer
// (appendInstanceData / persistInstanceData / DeleteInstance) as it happens; this
