package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Task-spawned session lifecycle (#2595).
//
// A cron task with no target session creates one session per fire, and af had no
// policy for what became of it. The run finished, the agent went idle, and the
// session then held its tmux session and its git worktree forever — four a day on
// the maintainer's box, until 12 of 17 live sessions were finished runs. The only
// thing standing between a schedule and unbounded growth was prose in the prompt
// ("finally, run af sessions archive --self"), which nothing enforces and
// `af tasks list` cannot show.
//
// The verb now lives on the task (task.OnComplete), and this is where it is
// applied.

// killSessionForLifecycle is the kill a declared teardown performs. A package var
// so a test can assert WHICH session the deferred goroutine names — the stale-id
// retarget it guards against is invisible if the only observable is that some
// session went away. Production points it at the real RPC and never reassigns it.
var killSessionForLifecycle = func(m *Manager, req KillSessionRequest, guard sessionTeardownGuard) error {
	_, err := m.killSessionRequestedBy(req, "task on_complete teardown", guard)
	return err
}

// archiveSessionForLifecycle is the archive twin of killSessionForLifecycle.
// Keeping the whole result behind one seam lets tests prove a committed warning
// is classified as a successful reap without performing a real archive.
var archiveSessionForLifecycle = func(m *Manager, req ArchiveSessionRequest, guard sessionTeardownGuard) error {
	_, _, err := m.archiveSessionGuarded(req, guard)
	return err
}

// taskLifecycleHookWait bounds how long a declared teardown waits for a session's
// post_worktree_commands to finish before giving up and leaving it in place. Long
// enough for an ordinary build/install hook, short enough that a hung one leaks a
// session rather than a goroutine.
const taskLifecycleHookWait = 10 * time.Minute

// runEndedIntoIdle reports whether this tick is the moment a task run finished
// with its session sitting idle and healthy.
//
// Three conditions, each excluding a different way the run marker can clear
// without a run having completed:
//
//   - the marker went true→false on THIS tick. taskRunActive flips once and
//     permanently, so this can fire at most once per session — a session cannot be
//     archived twice, and a session a user later adopts and works in is never
//     revisited. That is the difference between an edge and a standing predicate,
//     and it is why this is not a sweep: "task session whose run has ended" stays
//     true forever, including for the session someone is typing into right now.
//
//   - the session settled into LiveReady. CommitArchive also ends a run, and that
//     session is already archived with nothing left to do.
//
//   - startup did not settle terminal-unknown. This one is checked explicitly
//     even though the poll cannot currently deliver such a session here, and the
//     explicitness is the point. MarkStartupStateUnknown clears taskRunActive
//     DIRECTLY — not through a transition — and leaves liveness untouched, so an
//     uncertain create sits at LiveReady with its run marker clear and satisfies
//     both conditions above. What actually keeps it away from this function today
//     is refreshInstanceStatus's `!instance.Started()` early return, since the
//     same call also clears started. That is a correct outcome resting on a
//     distant, unrelated line: the daemon RETAINS an uncertain create's record so
//     an operator can inspect the workspace it may have left behind
//     (keepUncertainCreate), and reaping it would destroy exactly what that
//     retention exists to preserve. Stating the condition here means a future
//     refactor of the poll's early returns cannot silently authorize that.
func runEndedIntoIdle(instance *session.Instance, taskRunWasActive bool) bool {
	if !taskRunWasActive || instance.TaskRunActive() {
		return false
	}
	if instance.StartupStateUnknown() {
		return false
	}
	return instance.GetLiveness() == session.LiveReady
}

// deferTaskSessionLifecycleWhilePaused records that a run finished on the PAUSED
// poll path, so its declared lifecycle is applied once the attach ends.
//
// A run can finish while a TUI is attached full-screen: observeTaskRunWhilePaused
// settles the agent idle on the backstop tick, which ends the run and clears the
// marker there. The completion edge is then spent, and the ordinary hook — which
// only runs on the unpaused path — never sees it, so a declared archive/kill was
// skipped PERMANENTLY and the session leaked despite the policy. That is the exact
// failure #2595 exists to fix, reached through the one door the fix did not cover.
//
// Applying it inline instead is not the answer: the pause exists because the
// attached client owns the tmux server for the attach's duration, and tearing the
// session down under it is what the pause is there to prevent. So the intent is
// parked and drained on the first unpaused tick.
func (m *Manager) deferTaskSessionLifecycleWhilePaused(repoID string, instance *session.Instance, taskRunWasActive bool) {
	if !runEndedIntoIdle(instance, taskRunWasActive) || instance.TaskID == "" {
		return
	}
	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if m.deferredTaskLifecycle == nil {
		m.deferredTaskLifecycle = make(map[string]string)
	}
	// Keyed by session id, not by the map key alone: a kill and a same-titled
	// re-create would otherwise inherit the original's owed teardown.
	m.deferredTaskLifecycle[key] = instance.ID
	m.mu.Unlock()
}

// sweepDeferredTaskLifecycle drops parked intents whose session is gone or has
// been replaced.
//
// An intent is drained by a normal poll of the session that owes it, so a session
// killed (by the user, or with its project) while one is parked would never come
// back to collect — and the entry would sit in the map for the daemon's lifetime.
// That is the same reclamation the paused-poll lease sweep does for the same
// reason, and leaking a map entry inside the change that exists to stop leaks
// would be a poor joke.
//
// Runs under m.mu, from the same snapshot hold RefreshStatuses already takes.
func (m *Manager) sweepDeferredTaskLifecycleLocked() {
	for key, owedID := range m.deferredTaskLifecycle {
		inst := m.instances[key]
		if inst == nil || inst.ID != owedID {
			delete(m.deferredTaskLifecycle, key)
		}
	}
}

// applyDeferredTaskSessionLifecycle drains an intent parked while the session was
// attached, once it is being polled normally again.
//
// It re-checks that the session is STILL the one that finished and still idle. A
// user who attached, read the result, and then set the session working again has
// adopted it — that work is theirs, not the task's, and the same rule the edge
// enforces has to hold across the deferral.
func (m *Manager) applyDeferredTaskSessionLifecycle(repoID string, instance *session.Instance) {
	if instance.TaskID == "" {
		return
	}
	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	owedID, owed := m.deferredTaskLifecycle[key]
	if owed {
		delete(m.deferredTaskLifecycle, key)
	}
	m.mu.Unlock()
	if !owed {
		return
	}
	if instance.GetLiveness() != session.LiveReady || instance.TaskRunActive() {
		// The user picked the work back up during the attach. Drop the intent rather
		// than carrying it forward: a verb owed to a finished run must not land on
		// new work.
		return
	}
	// And the same re-validation the hook-wait teardown performs under the fence
	// (#3865) — a different session now holding the title, or a delivery since the
	// run ended. Two separate reads here, so this is an early-out and not the
	// decision: the authoritative one is taken under the fence inside
	// runTaskSessionLifecycle, which this call reaches through
	// applyTaskSessionLifecycleOnRunEnd below.
	if err := taskLifecycleStillOwed(instance, owedID, instance.AdoptionDeliveriesAtRunEnd(), instance.AdoptionDeliveries()); err != nil {
		m.info().Printf("task %s: dropping the lifecycle owed to session %q's finished run: %v", instance.TaskID, instance.Title, err)
		return
	}
	// The run genuinely ended and nothing has happened since, so this is the same
	// decision the edge would have made — taken now that the attach has released
	// the session. taskRunWasActive is passed as true because the edge it names
	// already happened, on the paused tick that parked this intent.
	m.applyTaskSessionLifecycleOnRunEnd(repoID, instance, true)
}

// applyTaskSessionLifecycleOnRunEnd applies the owning task's on_complete verb to
// a session whose run just finished. A no-op for every session that is not a
// task-spawned one whose run ended on this tick, and for the default keep — which
// is what makes this invisible to every task written before #2595.
//
// It runs on the poll goroutine but hands the actual teardown to a separate one:
// ArchiveSession relocates a worktree and KillSession removes one, both of which
// can take seconds on a large tree, and RefreshStatuses walks every session in
// series. Blocking here would stall liveness polling for every other session
// behind one teardown.
func (m *Manager) applyTaskSessionLifecycleOnRunEnd(repoID string, instance *session.Instance, taskRunWasActive bool) {
	if !runEndedIntoIdle(instance, taskRunWasActive) {
		return
	}
	taskID := instance.TaskID
	if taskID == "" {
		return
	}
	// Capture the STABLE ID here, not just the title. The teardown below runs
	// later on another goroutine, and ArchiveSession/KillSession fall back to
	// {Title, RepoID} only when ID is empty — so a title-only request lets a kill
	// and a same-titled re-create in the gap retarget the reap onto the
	// REPLACEMENT session. That is the unstable-identity verb class, and #2779 was
	// the last time a lifecycle op keyed on a title reached the wrong worktree.
	sessionID := instance.ID
	title := instance.Title
	// The adoption baseline was pinned by the completion transition itself, in the
	// same critical section that cleared taskRunActive (#3865) — NOT read here,
	// which is the window #2953 left open with persistPollChange's storage I/O
	// inside it. This is a read of a value that is already fixed.
	adoptedAt := instance.AdoptionDeliveriesAtRunEnd()
	hooksDone := instance.PostWorktreeHooksDone()
	verb, err := m.taskSessionLifecycle(repoID, taskID)
	if err != nil {
		// An unreadable or unscopable task store is not permission to tear a
		// session down. Keep it — the conservative outcome, and the same one an
		// older daemon produced — and say so once, on the tick that could not
		// decide. A later run's completion asks again.
		m.warn().Printf("could not read the session lifecycle for task %s (session %q): %v; leaving the session in place",
			taskID, title, err)
		return
	}
	if verb == task.OnCompleteKeep {
		return
	}
	go m.runTaskSessionLifecycle(repoID, sessionID, title, taskID, verb, hooksDone, adoptedAt)
}

// taskSessionLifecycle resolves the on_complete verb for one task in a repo.
// A task that no longer exists yields keep: its sessions outlive it, and deleting
// a task must not retroactively authorize destroying the work its runs produced.
func (m *Manager) taskSessionLifecycle(repoID, taskID string) (string, error) {
	tasks, bindingUpdates, err := loadTasksForRepoID(repoID)
	// Publish before propagating, for the reason loadEnabledTaskTargets documents:
	// the load commits backfilled bindings durably even when it then returns a
	// scope error, and nothing else republishes them.
	for _, updated := range bindingUpdates {
		m.publishEvent(agentproto.EventTaskUpdated, updated)
	}
	if err != nil {
		return "", err
	}
	for _, t := range tasks {
		if t.ID == taskID {
			return t.SessionLifecycle(), nil
		}
	}
	return task.OnCompleteKeep, nil
}

// taskLifecycleStillOwed reports WHY a teardown owed to one completed run may no
// longer act on the session the manager holds now, or nil when it still may. It
// is the single re-validation both routes into a teardown use — the hook-wait
// goroutine, under the fence, and the deferred drain's early-out.
//
// deliveries is passed in rather than read here because WHERE it is read is the
// whole point: the decision that destroys reads it through CloseAdoptionFence,
// in one critical section with the fence shutting, so a delivery cannot land
// between the read and the destruction.
//
// The two conditions are the identity and the adoption signal, and nothing else.
// Liveness is deliberately absent: a user's turn that starts and settles reads
// LiveReady again, so a level cannot separate the task's idle from the user's
// (#2953's first P1). The deferred route still asks its own liveness question
// before calling this, because there the user typed into an ATTACHED tmux, which
// reaches no agent-server entry point and so moves no count.
func taskLifecycleStillOwed(current *session.Instance, sessionID string, adoptedAt, deliveries uint64) error {
	if current == nil {
		return errors.New("the session is no longer registered")
	}
	if current.ID != sessionID {
		return fmt.Errorf("session id %s now holds this title, not %s", current.ID, sessionID)
	}
	if deliveries != adoptedAt {
		return fmt.Errorf("its delivery count moved from %d to %d since its run ended, so the work is the user's now", adoptedAt, deliveries)
	}
	return nil
}

// testHookTaskLifecycleGuardPassed runs INSIDE the teardown's fence, after the
// re-validation has passed and the adoption fence is shut, and before the
// destructive operation touches anything. It is the seam that makes the
// constraint-5 race expressible: a test pauses the teardown exactly in the window
// #2953 could never close and delivers into it. No-op in production.
var testHookTaskLifecycleGuardPassed = func() {}

// runTaskSessionLifecycle performs the teardown on its own goroutine.
//
// # The fence, and why the decision is taken inside it (#3865)
//
// Four earlier attempts re-CHECKED the completion decision before calling
// ArchiveSession/KillSession and were all wrong for one reason: the check and the
// destruction were not serialized against each other, so a delivery could pass
// its own guards and land in the gap between them. What follows is not a fifth
// comparison. The teardown now takes the same fence delivery takes, re-validates
// under it, and only then destroys:
//
//	                 ┌─────────────── inside Archive/Kill ───────────────┐
//	hook wait ──────▶│ op-lock held · killsInFlight[key] claimed          │
//	                 │   guard(current):                                 │
//	                 │     CloseAdoptionFence()  ─ shut and read, one i.mu│
//	                 │     compare id + deliveries against the baseline   │
//	                 │   ── testHookTaskLifecycleGuardPassed ──           │
//	                 │   destroy, or return and stand down               │
//	                 └───────────────────────────────────────────────────┘
//
// Both delivery paths are covered, by different halves of that fence:
//
//   - Manager.SendPrompt takes killsInFlight + the op-lock. It either completes
//     before the guard (its bump is inside the count the guard reads) or is
//     refused by the claim this teardown is holding.
//   - Browser/TUI PTY input reaches InputTab with NO manager lock, so the op-lock
//     says nothing about it. The adoption fence does: CloseAdoptionFence and
//     NoteAdoptionDelivery are one i.mu section each, so a keystroke either
//     counted before the guard read (stand down) or is refused (ErrAdoptionFenced).
//     See session/adoption_fence.go for that argument in full.
//
// Standing down leaves the session exactly where it is — the recoverable
// outcome, and the same one the hook-wait timeout already produces.
//
// It reuses ArchiveSession/KillSession rather than reaching for the primitives
// underneath, so a task-driven teardown is the SAME operation a user's
// `af sessions archive` is: the same killsInFlight + op-lock serialization
// (#2779), the same refusal when a task still targets the session, the same
// events. A policy that tore down sessions through a private path would be a
// second lifecycle implementation, and the two would drift.
//
// Both requests carry the stable id, so a session killed and re-created under the
// same title between the completion edge and this call cannot be reaped in the
// original's place — the resolver only falls back to {Title, RepoID} when ID is
// empty.
func (m *Manager) runTaskSessionLifecycle(repoID, sessionID, title, taskID, verb string, hooksDone <-chan struct{}, adoptedAt uint64) {
	// post_worktree_commands can still be running: the agent's readiness and the
	// hook run are deliberately concurrent (task.WaitForReady does not charge a
	// slow build hook against the startup budget), so a short task can finish while
	// its own provisioning is mid-flight. Archiving would MOVE the worktree out
	// from under those hooks and killing would REMOVE it, so wait for them.
	//
	// Bounded, because this must not become a way for a hung hook to wedge the
	// reap forever — the same reasoning the concurrency cap gives for not waiting
	// on hooks to release a slot. On timeout the session is left in place and says
	// why, which is the recoverable outcome.
	if hooksDone != nil {
		select {
		case <-hooksDone:
		case <-time.After(taskLifecycleHookWait):
			m.warn().Printf("task %s: post-worktree hooks for session %q have run for over %s; leaving the session in place rather than tearing down a worktree they may still be writing to",
				taskID, title, taskLifecycleHookWait)
			return
		}
	}
	// The fence is shut by the guard and reopened as soon as the operation it
	// covers is over, whichever way that went. Reopening is unconditional because
	// the alternative fails badly: an archive refused for some unrelated reason
	// would leave a live session permanently unable to accept a keystroke. After a
	// teardown that DID go through, the session is out of the manager and the
	// reopen is a no-op for it.
	//
	// fenced and standDown are written and read on this goroutine only — the guard
	// is invoked synchronously by the destructive call below, not by one of its
	// workers.
	var fenced *session.Instance
	var standDown error
	defer func() {
		if fenced != nil {
			fenced.ReopenAdoptionFence()
		}
	}()
	guard := func(current *session.Instance) error {
		var deliveries uint64
		if current != nil {
			deliveries = current.CloseAdoptionFence()
		}
		if err := taskLifecycleStillOwed(current, sessionID, adoptedAt, deliveries); err != nil {
			// Reopened HERE rather than left to the defer: a refusal means the
			// destructive call returns without touching anything, so there is nothing
			// left to fence, and the user whose delivery caused this stand-down should
			// not have their next keystroke refused while this goroutine finishes
			// logging.
			if current != nil {
				current.ReopenAdoptionFence()
			}
			standDown = err
			return err
		}
		fenced = current
		testHookTaskLifecycleGuardPassed()
		return nil
	}

	var err error
	switch verb {
	case task.OnCompleteArchive:
		err = archiveSessionForLifecycle(m, ArchiveSessionRequest{ID: sessionID, Title: title, RepoID: repoID}, guard)
	case task.OnCompleteKill:
		err = killSessionForLifecycle(m, KillSessionRequest{ID: sessionID, Title: title, RepoID: repoID}, guard)
	default:
		// Unreachable: ValidateTrigger refuses an unknown verb on write, and
		// SessionLifecycle canonicalizes. Log rather than guess — picking a verb
		// here would be inventing destructive intent from a value nothing accepted.
		m.warn().Printf("task %s declares an unknown on_complete %q; leaving session %q in place", taskID, verb, title)
		return
	}
	// Reopened at the earliest correct moment rather than only on the way out. The
	// remaining window — between the destructive op releasing its killsInFlight
	// claim and returning here — carries no I/O, and a delivery landing inside it
	// is refused with an error and retried rather than lost.
	if fenced != nil {
		fenced.ReopenAdoptionFence()
	}
	if standDown != nil {
		// The one line the recoverable outcome gets, naming which of the two
		// conditions failed. Not a warning: a user adopting a finished task session
		// is ordinary, and the verb going unapplied is the correct result.
		m.info().Printf("task %s: not applying on_complete=%s to session %q — %v; leaving the session in place",
			taskID, verb, title, standDown)
		return
	}
	if isMutationCommitted(err) {
		m.warn().Printf("task %s: applied on_complete=%s to session %q with a committed warning: %v",
			taskID, verb, title, err)
		return
	}
	if err != nil {
		// Failing to reap is not a failure of the RUN, which already succeeded, so
		// this never touches the task's last-run status. The session stays where it
		// is and stays visible, which is the recoverable outcome; a user can finish
		// the teardown by hand.
		m.warn().Printf("task %s: could not %s session %q after its run finished: %v", taskID, verb, title, err)
		return
	}
	m.info().Printf("task %s: applied on_complete=%s to session %q after its run finished", taskID, verb, title)
}
