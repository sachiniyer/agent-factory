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

// taskRunPollBackstop bounds how long an attached TUI may hide a task run's
// COMPLETION from the watch-task concurrency cap (#1892).
//
// The pause (#1160) exists because a probe is NEEDLESS while a user is looking at
// the pane: they can see the session, so the status dot does not need refreshing,
// and capture-pane contends with the attach. The cap broke that premise. A task
// run's slot is released when its agent goes idle, and the ONLY observer of
// agent-idleness is the pane — an agent that finishes a turn does not exit, so
// there is no process/tmux signal to listen for (see tmux HasUpdated: idleness is
// literally "the pane hash did not change"). So while the poll was paused, the
// completion was never seen, the slot was never released, and the task quietly
// stopped launching sessions with no error at all — the cap simply read as full.
//
// This is deliberately a backstop, not a return to per-tick probing: the attach
// keeps its fast path and pays one probe per interval instead of one per tick. It
// is also self-limiting — it applies ONLY while a task run is in flight, so the
// probe that observes the run finishing is also the last one it forces.
//
// A var so tests can shrink it; production never reassigns.
var taskRunPollBackstop = 30 * time.Second

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
	delete(m.taskRunProbeDue, key)
	m.pausedMu.Unlock()
}

// taskRunBackstopDue reports whether a PAUSED session with an in-flight task run
// is due its backstop observation, recording the next one when it is (#1892).
//
// The first paused tick only arms the timer, so an attach still gets its quiet
// window; the completion is then observed within one taskRunPollBackstop. Shares
// pausedMu with pausedPolls: same lifetime, same lock discipline, and never m.mu
// (the poll deliberately runs its probes with that released).
func (m *Manager) taskRunBackstopDue(key string) bool {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	now := nowFunc()
	due, armed := m.taskRunProbeDue[key]
	if !armed || now.Before(due) {
		if !armed {
			m.taskRunProbeDue[key] = now.Add(taskRunPollBackstop)
		}
		return false
	}
	m.taskRunProbeDue[key] = now.Add(taskRunPollBackstop)
	return true
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

// observeTaskRunWhilePaused runs ONE bounded observation of an attached session
// whose task run is still in flight, so the concurrency cap learns the run ended
// even though the poll is paused (#1892). Called only from the pause branch of
// refreshInstanceStatus, at most once per taskRunPollBackstop.
//
// It can END a run and it can never conclude DEATH, and both halves are
// deliberate:
//
//   - It ends a run by resolving the idle branch exactly as the normal poll does
//     (resolveIdleLiveness → Ready, or LimitReached when the pane shows a limit
//     banner). That transition is what releases the slot.
//   - It never marks the session Lost, and never accumulates a remote-loss
//     failure. The attach is positive evidence of life — a client is streaming
//     this session's PTY, which no dead session can serve — and #1794's warning
//     applies with full force here: a pause outlasts remoteLostGracePeriod, so
//     letting a blip during an attach settle Lost would feed
//     RestoreLostSessions a session the user is typing into, and a remote Recover
//     RE-PROVISIONS, orphaning the live sandbox and its unpushed commits. A probe
//     that cannot answer therefore tells us nothing and is dropped, exactly as the
//     plain pause does.
//
// It also does not TapEnter: AutoYes is the normal poll's business, and a user is
// attached and can answer their own prompt. Keeping this to the one question the
// cap needs is what keeps a bounded probe from quietly becoming the whole poll.
func (m *Manager) observeTaskRunWhilePaused(repoID, key string, instance *session.Instance) {
	before := instance.GetLiveness()
	beforeReset, _ := instance.LimitResetAt()
	obs, err := instance.AgentServer().Snapshot()
	// Whatever happened, no loss episode survives an attach (see above).
	m.clearRemoteLoss(key)
	if err != nil || obs.Updated || obs.HasPrompt {
		// Unanswerable, still working, or waiting on the user: either way the run
		// has not finished, so there is nothing for the cap to learn this tick.
		return
	}
	// Idle output. The normal poll would probe liveness here to tell a healthy idle
	// session from a vanished one; this path deliberately does not, because it must
	// never conclude death. The attach already answers that question.
	m.resolveIdleLiveness(instance, obs.Content)
	m.persistPollChange(repoID, instance, before, beforeReset)
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

	// Drop debounce state for sessions that are gone or replaced, colocated with
	// the pass that creates it (#1794).
	m.sweepRemoteLossStates()

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
//   - a failed observation probe (only a remote agent-server can error) →
//     Lost when a second, independent Alive() probe also fails, so an
//     unreachable sandbox stops reading as healthy (#1782);
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
//
// Every early return below SKIPS the probe, and each one therefore breaks the
// remote-loss debounce's "consecutive" contract: a tick we did not observe tells
// us nothing, so a failure before the gap and a failure after it are not
// consecutive and must not be summed (#1794). Each skip clears the episode —
// a run of unanswerable probes has to be unbroken to mean anything, and the
// alternative is a stale count that survives an arbitrary blind window and lets
// ONE later blip tip a healthy session into a destructive re-provision.
func (m *Manager) refreshInstanceStatus(repoID string, instance *session.Instance) {
	if instance == nil || !instance.Started() {
		return
	}
	// The debounce is keyed by stable instance ID, never repo/title: a same-title
	// successor must not inherit a dead predecessor's failures (#1794).
	key := remoteLossKey(repoID, instance)
	if instance.UserKilled() {
		// A surviving kill-intent tombstone (#1108) means a previous
		// KillSession was interrupted after committing to the kill. The only
		// valid future for this session is finishing that teardown — never
		// probing it, never marking it Lost, never restoring it.
		m.clearRemoteLoss(key)
		m.finishUserKill(repoID, instance)
		return
	}
	if instance.GetInFlightOp() != session.OpNone {
		// An op is mid-flight (archive/restore teardown, create/kill overlay): the
		// poll must not probe a session whose tmux is being spun up/torn down and
		// mark it Lost — the op's executor writes the settled liveness. Replaces
		// the old Loading/Deleting skip (#1195). The op may also be REPLACING the
		// runtime under us, which is the other reason its failure history dies here.
		m.clearRemoteLoss(key)
		return
	}
	if instance.GetLiveness() == session.LiveArchived {
		// Archived (#1028): no tmux to probe, inert (started=false) so already
		// skipped by !Started above — belt-and-suspenders against a future change.
		m.clearRemoteLoss(key)
		return
	}
	if m.isPollPaused(repoID, instance.Title) {
		// EXCEPT when a task run is still in flight: the concurrency cap (#1892)
		// releases that run's slot when the agent goes idle, and this poll is the
		// only thing that can see that happen. Left un-probed for the whole attach,
		// a run that finished under the user's eyes never released its slot and the
		// task silently stopped launching sessions — no error, because the cap just
		// read as full. Observe on a bounded backstop instead; see
		// observeTaskRunWhilePaused for why it can end a run but never conclude
		// death, and taskRunPollBackstop for why this is not a return to per-tick
		// probing.
		if instance.TaskRunActive() && m.taskRunBackstopDue(remoteLossKey(repoID, instance)) {
			m.observeTaskRunWhilePaused(repoID, key, instance)
			return
		}
		// A TUI is attached full-screen to this instance (#1160). It owns the
		// shared tmux server for the attach duration; the daemon's capture-pane
		// liveness probe here would needlessly contend with the live attach and
		// hurt input responsiveness (Fix A follow-up to #1157). Skip the probe.
		// The status is left UNCHANGED — a paused instance is known-attached-and-
		// alive, never marked Lost (#1108): it has not vanished. Leak-safe: the
		// pause is lease-bounded (statusPollLease), so a crashed TUI that never
		// sends Resume auto-resumes within one lease and real death is detected
		// on the next tick — the pause can never permanently blind the daemon.
		//
		// The attach is also positive evidence: the client is streaming this
		// session's PTY off the sandbox, which no dead sandbox can serve. Dropping
		// the episode here is not merely conservative, it is what the counter's
		// definition demands — a pause can outlast remoteLostGracePeriod (the TUI
		// renews it for the whole attach), so a surviving count would let the first
		// blip after detach re-provision a sandbox the user was just typing into.
		m.clearRemoteLoss(key)
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
		// The observation probe failed. The local agent-server never errors here,
		// so this is the REMOTE runtime's path (#1592 Phase 4): the REST call to
		// the in-sandbox agent-server failed — a dead container, a dropped ssh
		// forward, an agent-server crash.
		//
		// A failed probe is not by itself proof of death: a transient blip against
		// a healthy sandbox errors identically, and acting on one is destructive —
		// Lost feeds RestoreLostSessions in this same tick, whose remote Recover
		// RE-PROVISIONS a fresh sandbox and orphans the running one along with its
		// unpushed commits (#1794). So require DURABLE failure (see remoteloss.go)
		// before even considering Lost.
		//
		// Lost, not Dead (#1108): no kill intent is on record, so the session
		// vanished out from under a live record and stays recovery-eligible.
		// Without this the remote session kept its last-known liveness
		// (Running/Ready) forever while its agent-server was gone, so the TUI
		// showed a healthy row for a dead session (#1782).
		m.settleRemoteProbeFailure(repoID, key, instance, before, beforeReset)
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
	// The Snapshot answered, so the transport works and any loss episode is over.
	// This is the ONLY thing the debounce tracks — see remoteloss.go: it counts
	// unanswerable probes, not "looks dead" observations.
	m.clearRemoteLoss(key)
	switch {
	case updated:
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
	case hasPrompt:
		// A waiting prompt with otherwise-unchanged output: leave the status for
		// the next tick to resolve, exactly as runMetadataTick did. The
		// prompt-tap already fired above regardless of `updated`.
	default:
		// Snapshot returned (false,false), which a healthy idle session and a
		// dead one both produce — indistinguishable on their own. Probe liveness
		// only on this idle branch so a vanished session is marked Lost and
		// rendered distinctly rather than repainted as a green Ready dot it can
		// no longer back (#935). Lost, not Dead (#1108): there is no kill
		// intent on record, so the session vanished out from under a live
		// record — an outage/reboot casualty that is recovery-eligible, not a
		// corpse the user wanted gone.
		switch probe := probeLiveness(instance, as); probe {
		case probeDead:
			// AUTHORITATIVE: the agent-server (or local tmux) was asked and
			// reports the agent gone. No debounce — the evidence is present and
			// bad, not absent. #935's immediacy is unchanged for both runtimes.
			_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
		case probeUnknown:
			// A REMOTE probe that never answered. It says nothing about the agent,
			// and Lost here feeds a re-provision that orphans a possibly-live
			// sandbox, so it only counts toward the debounce (#1794) — never
			// settles anything by itself.
			//
			// Reaching this means the Snapshot answered on this same tick while the
			// Alive probe did not: the transport worked a moment ago, so this is a
			// blip between two calls, and the clear above has already reset the
			// episode. The count therefore cannot climb here on its own — by
			// design. A real outage takes Snapshot down too, and that path
			// (settleRemoteProbeFailure) is where an episode accumulates. Recording
			// it anyway keeps one honest definition of the counter — "probes that
			// could not be answered" — so a degrading transport starts its episode
			// at the first unanswered probe rather than the first failed Snapshot.
			if m.noteRemoteProbeFailure(key, instance.Title) {
				_ = instance.Transition(session.ObserveLiveness(session.LiveLost))
			}
		case probeAlive:
			// Idle output: settle to Ready, or LimitReached when the pane shows a
			// usage-limit banner for a claude/codex session (#1146). content is
			// HasUpdated's capture (no re-capture); see resolveIdleLiveness.
			m.resolveIdleLiveness(instance, content)
		}
	}

	// Persist a liveness OR usage-limit reset-time change (#1146); see limit.go.
	m.persistPollChange(repoID, instance, before, beforeReset)
}

// SaveInstances writes the manager's authoritative in-memory instances to disk
// as a straight per-repo marshal (#960 PR 4). With the daemon the sole writer of
// instances.json there is no competing snapshot to reconcile, so this is no
// longer a merge. Every mutation already persists through a targeted writer
// (appendInstanceData / persistInstanceData / DeleteInstance) as it happens; this
