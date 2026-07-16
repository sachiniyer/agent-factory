package daemon

import (
	"errors"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// Watch-task concurrency limit (#1892).
//
// A watch task that creates a session per event had no bound on how many of
// those sessions could be in flight at once. The reporter's DLQ watcher spawned
// five sessions in ten seconds — each kicking off a post-worktree hook running
// `make dev_install` — while a userland monitor tried to hold a cap of three by
// listing sessions, matching a generated title prefix, and interpreting
// liveness. It overshot, because a session is invisible to that reconstruction
// during the very window that matters: creation.
//
// What is bounded here is NOT concurrent CreateSession calls. Those are already
// serial per task — the watcher's single reader goroutine delivers synchronously
// (see the run loop in watcher.go), so at most one create is ever in flight, and
// "at most K concurrent spawns" would be trivially true for any K >= 1. What
// piles up is SESSIONS: CreateSession returns as soon as the agent is ready and
// its prompt is sent, while the post-worktree hook is still draining
// asynchronously and the agent is still working. So the limit counts a task's
// in-flight SESSIONS, and an event that would exceed it parks.
//
// The three pieces, none of them new machinery:
//
//   - Counting is a PROJECTION over the daemon's own instances, filtered by the
//     task_id persisted on each session (#1892 requires provenance, not a title
//     prefix) and by session.ClassifyActivity — the same busy/idle state machine
//     `af sessions watch` reads. A projection cannot drift out of sync with
//     reality the way a counter does: a counter restarts at zero on a daemon
//     restart while K sessions are still live (silently over-admitting), and one
//     leaked decrement wedges the task forever.
//
//   - Admission is decided inside reserveCreate, under the manager lock, next to
//     the title reservation whose release() already outlives the instance's
//     registration in m.instances. That ordering hands a slot from the
//     reservation to the real session with no gap for a burst to slip through.
//
//   - A refused delivery reuses the errTargetBusy park path (#1586) verbatim: the
//     event is queued in the durable FIFO backlog (#1129) and retried until a slot
//     frees. Nothing is dropped, which is what the issue requires.

// atConcurrencyLimitErrText is the wire-visible text of the admission refusal.
// net/rpc flattens server-side errors into plain strings, so the task delivery
// path — which reaches the manager through the daemon's own control socket —
// cannot errors.Is against a sentinel; isAtConcurrencyLimitErr matches this text
// instead. Same idiom, and same reason, as daemonStartingErrText.
const atConcurrencyLimitErrText = "task is at its max_concurrent_runs limit; delivery deferred until a session finishes"

// errAtConcurrencyLimit is the in-process sentinel the watcher's live and replay
// paths match on to park an event. It is a sibling of errTargetBusy: both mean
// "this delivery cannot land right now, through no fault of the pipeline", and
// both are parked rather than failed — never counted against the delivery-failure
// alarm (#1238), never logged as an error.
var errAtConcurrencyLimit = errors.New(atConcurrencyLimitErrText)

// isAtConcurrencyLimitErr reports whether an error — including one flattened by
// net/rpc on its way back from the daemon — is the admission refusal.
func isAtConcurrencyLimitErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), atConcurrencyLimitErrText)
}

// A slot is held by every session this task spawned that ClassifyActivity calls
// ActivityPending: mid-create (including while its post-worktree hooks still
// run), running, or parked at a usage limit the daemon auto-resumes (#1146). It
// is released the moment the session goes idle, or reaches a state it cannot
// leave on its own (lost/dead/archived).
//
// Releasing at idle rather than at archive is deliberate. A slot that only freed
// when a human archived the session would wedge the task until someone did —
// and the backlog would then age out against the event queue's retention bound,
// turning a concurrency cap into silent event loss. Idle also means the agent
// has finished the work the event asked for, which is what the cap is about.
//
// The symmetric bound: a slot frees at idle even if a post-worktree hook is
// still draining. Holding it until PostWorktreeHooksDone would be more precise
// about machine load, but a hook that never exits would leak the slot forever —
// a permanently wedged watcher, strictly worse than the unbounded spawning this
// limit exists to fix. The agent's own work outlasts the hook in the reported
// case anyway.

// taskRunReservationKey scopes an in-flight reservation to the task AND its repo,
// matching how countTaskRunsLocked filters live sessions. Keying by task id alone
// would be correct today — task ids are globally unique and a task maps to one
// repo — but a bare-id key would silently start counting across repos the instant
// that invariant changed, so the reservation and the live count use the same
// (repo, task) scope.
func taskRunReservationKey(repoID, taskID string) string {
	return repoID + "\x00" + taskID
}

// countTaskRunsLocked reports how many sessions the task currently has in flight
// in the repo: reserved creates that have not yet registered an instance, plus
// live sessions ClassifyActivity calls pending. Callers hold m.mu and have
// already called refreshLocked, so m.instances reflects what is on disk — which
// is what makes the count survive a daemon restart with sessions still live.
func (m *Manager) countTaskRunsLocked(repoID, taskID string) int {
	inFlight := m.reservedTaskRuns[taskRunReservationKey(repoID, taskID)]
	for key, inst := range m.instances {
		if inst == nil || inst.TaskID != taskID {
			continue
		}
		if rid, _ := splitDaemonInstanceKey(key); rid != repoID {
			// Scoped to the task AND the repo (#1892): counting another project's
			// sessions would let them starve this one.
			continue
		}
		// Read the two axes directly rather than serializing the whole instance:
		// this runs under m.mu, and ToInstanceData walks tabs/worktree/PR state per
		// session. Snapshot keeps its serialize outside the lock for the same
		// reason, and an AF home can hold hundreds of sessions.
		if session.ClassifyInstanceActivity(inst) == session.ActivityPending {
			inFlight++
		}
	}
	return inFlight
}

// admitTaskRunLocked refuses a task delivery that would exceed its cap. It is
// READ-ONLY — it reserves nothing — so it can run early in reserveCreate, before
// any title mutation (the archived-name-reuse rename), and refuse without
// leaving a half-applied create behind. reserveTaskRunLocked records the slot
// only once the create is committed to succeeding. reserveCreate holds m.mu
// unbroken between the two, so no other create can change the count in the gap.
func (m *Manager) admitTaskRunLocked(repoID, taskID string, limit int) error {
	if taskID == "" || limit <= 0 {
		// No provenance or no cap: unlimited, exactly as before #1892. Zero is the
		// default for every task written before this field existed, so an absent
		// cap can never change an existing task's behavior.
		return nil
	}
	if m.countTaskRunsLocked(repoID, taskID) >= limit {
		return errAtConcurrencyLimit
	}
	return nil
}

// reserveTaskRunLocked records an admitted create that has not yet registered its
// instance in m.instances, so a burst cannot count the same zero N times and
// admit them all. It runs only on the committed-to-succeed path, after
// admitTaskRunLocked passed under the same lock hold. Callers hold m.mu.
func (m *Manager) reserveTaskRunLocked(repoID, taskID string, limit int) {
	if taskID == "" || limit <= 0 {
		return
	}
	m.reservedTaskRuns[taskRunReservationKey(repoID, taskID)]++
}

// releaseTaskRunLocked drops a slot reserved by reserveTaskRunLocked. It runs
// from the same release() the title reservation uses, which CreateSession
// defers — so it fires only after the new instance is registered in m.instances
// and has begun holding the slot on its own. The momentary overlap (reservation
// and session both counted) is deliberate: over-counting for an instant refuses
// one extra event, while a gap would admit one too many. Callers hold m.mu.
func (m *Manager) releaseTaskRunLocked(repoID, taskID string) {
	if taskID == "" {
		return
	}
	key := taskRunReservationKey(repoID, taskID)
	if n := m.reservedTaskRuns[key]; n > 1 {
		m.reservedTaskRuns[key] = n - 1
		return
	}
	delete(m.reservedTaskRuns, key)
}
