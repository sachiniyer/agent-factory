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
	// The parked intent is in-memory; the marker is its durable form, so a
	// restart before the attach ends cannot lose the obligation (#4162).
	m.fileOwedTaskLifecycle(repoID, instance)
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
		// new work. The durable marker comes down with it (#4162).
		m.dischargeOwedTaskLifecycle(repoID, owedID, instance.Title)
		return
	}
	// The durable marker is the obligation this drain exists for (#4162): a
	// delivery that already discharged it also counts as adoption evidence, so
	// a parked intent with no marker left means the session is the user's.
	marker := instance.OwedOnComplete()
	if marker == nil {
		return
	}
	// Pane churn after the filing is the attach-path adoption evidence the
	// delivery counter cannot see — typing into an attached tmux reaches no
	// agent-server entry point — and unlike the count it survives a restart.
	if churn := instance.LastPaneChurnAt(); churn.After(marker.FiledAt) {
		m.info().Printf("task %s: dropping the lifecycle owed to session %q's finished run: pane churn postdates the obligation filed at %s — the work is the user's now",
			instance.TaskID, instance.Title, marker.FiledAt.Format(time.RFC3339))
		m.dischargeOwedTaskLifecycle(repoID, owedID, instance.Title)
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
		m.dischargeOwedTaskLifecycle(repoID, owedID, instance.Title)
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
		// session down. Keep it — the conservative outcome — but keep the
		// obligation TOO: the decision is owed whether or not the store answered
		// this tick, and a restart must re-ask rather than forget (#4162). Warn
		// only when the marker is first filed; a re-drive of an already-durable
		// obligation re-asks every poll and must not spam.
		if instance.OwedOnComplete() == nil {
			m.warn().Printf("could not read the session lifecycle for task %s (session %q): %v; leaving the session in place and recording the obligation so a restart can ask again",
				taskID, title, err)
			m.fileOwedTaskLifecycle(repoID, instance)
		}
		return
	}
	if verb == task.OnCompleteKeep {
		// Keep is a decision: discharge the marker a pause or a restart parked
		// (#4162). An edge-path keep never filed one, so this is a no-op there.
		if instance.OwedOnComplete() != nil {
			m.dischargeOwedTaskLifecycle(repoID, sessionID, title)
		}
		return
	}
	// Durable BEFORE the wait: the worker below is exactly what a shutdown
	// drops, so the obligation it carries must already be on disk (#4162).
	m.fileOwedTaskLifecycle(repoID, instance)
	// One worker per obligation per generation: the marker stays set for the
	// whole hook wait, so a refresh re-arm can park the intent again while this
	// one is still waiting — the claim is what keeps that a re-drive, not a
	// second teardown.
	if !instance.ClaimOwedDrain() {
		return
	}
	if !m.launchBackgroundMutation(func(stop <-chan struct{}) {
		defer instance.ReleaseOwedDrain()
		m.runTaskSessionLifecycleUntil(stop, repoID, sessionID, title, taskID, verb, hooksDone, adoptedAt)
	}) {
		// Shutdown closed admission: no worker exists to drive this, so the
		// claim comes back — the durable marker still re-arms next generation.
		instance.ReleaseOwedDrain()
	}
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
	m.runTaskSessionLifecycleUntil(nil, repoID, sessionID, title, taskID, verb, hooksDone, adoptedAt)
}

func (m *Manager) runTaskSessionLifecycleUntil(stop <-chan struct{}, repoID, sessionID, title, taskID, verb string, hooksDone <-chan struct{}, adoptedAt uint64) {
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
		timer := time.NewTimer(taskLifecycleHookWait)
		defer timer.Stop()
		select {
		case <-hooksDone:
		case <-stop:
			// The obligation stays durable: the next daemon generation re-arms
			// the drain and re-waits on whatever hook run it adopts (#4162).
			return
		case <-timer.C:
			m.warn().Printf("task %s: post-worktree hooks for session %q have run for over %s; leaving the session in place rather than tearing down a worktree they may still be writing to",
				taskID, title, taskLifecycleHookWait)
			// "Leave it" is a decision, so it is durable: the marker comes down
			// and the session stays put, visibly, rather than re-waiting the
			// same stuck hooks after every restart.
			m.dischargeOwedTaskLifecycle(repoID, sessionID, title)
			return
		}
	}
	select {
	case <-stop:
		return
	default:
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
		var marker *session.PendingOnCompleteData
		if current != nil {
			// The shut fence makes these reads atomic-equivalent: once it
			// closes, no delivery can bump the count or clear the marker, and a
			// delivery that beat it discharged both in the same section that
			// counted it — the two reads can never disagree.
			deliveries = current.CloseAdoptionFence()
			marker = current.OwedOnComplete()
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
		if marker == nil {
			if current != nil {
				current.ReopenAdoptionFence()
			}
			standDown = errors.New("the on_complete obligation was already discharged")
			return standDown
		}
		if current.GetLiveness() == session.LiveArchived {
			// Archived by hand — or by a concurrent op — while the worker
			// waited. The session is already at its kept-end state, so no owed
			// verb may run on it; on_complete=kill least of all, which would
			// destroy the record an explicit archive just chose to keep.
			current.ReopenAdoptionFence()
			standDown = errors.New("the session was archived while the teardown waited")
			return standDown
		}
		if churn := current.LastPaneChurnAt(); churn.After(marker.FiledAt) {
			// The same attach-path adoption the deferred drain checks: typing
			// into an attached tmux moves the pane without moving the delivery
			// count, and the timestamp is durable — it still answers after a
			// restart wiped the counter (#4162).
			if current != nil {
				current.ReopenAdoptionFence()
			}
			standDown = fmt.Errorf("pane churn at %s postdates the obligation filed at %s, so the work is the user's now",
				churn.Format(time.RFC3339), marker.FiledAt.Format(time.RFC3339))
			return standDown
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
		// A stand-down IS the decision — settle the marker so the next daemon
		// generation does not re-drive a teardown the evidence already refused.
		// For the delivery path this is a no-op: the delivery discharged it.
		m.dischargeOwedTaskLifecycle(repoID, sessionID, title)
		return
	}
	// Whatever the op committed settles the obligation on disk: archive's own
	// persist writes the row without the marker (the archived serialize gate)
	// and kill deletes the row outright. Clearing memory too keeps a later
	// unarchive from resurrecting an obligation the disk no longer carries —
	// and writes nothing, so a just-deleted kill row cannot be written back.
	if err == nil || isMutationCommitted(err) {
		if fenced != nil {
			fenced.SetOwedOnComplete(nil)
		}
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

// ── The durable obligation (#4162) ───────────────────────────────────────────
//
// A declared on_complete teardown used to live in exactly one place: the
// lifecycle goroutine parked on the post-worktree hook wait. A shutdown dropped
// the goroutine, the completion edge behind it was already spent
// (task_run_active is persisted false and flips only once), and nothing after a
// restart could re-derive that a teardown was owed — the session and its
// worktree simply stayed, forever, one leak per interrupted run.
//
// The fix is a marker on the session's own row: InstanceData.PendingOnComplete,
// filed BEFORE the wait begins and discharged only by a decision. These four
// helpers are its whole vocabulary: fileOwed writes it, dischargeOwed settles
// it, installOwedTaskLifecycleNotify makes an adoption's discharge durable, and
// armOwedTaskLifecyclesLocked is what a restart does with a marked row.

// fileOwedTaskLifecycle records the durable form of the teardown obligation on
// the session's own instances.json row — BEFORE the hook wait a shutdown would
// otherwise drop. Refiling is a no-op: the earliest FiledAt stays the adoption
// watermark, and a marker that is already set is already durable (filed this
// generation or restored from disk).
//
// The filed-or-refuse decision is taken under i.mu by FileOwedOnCompleteIfNotDischarged
// so a pre-filing adoption delivery in the paused-path window (#4162's race) is the
// whole stand-down signal: it cannot clear a marker that does not exist yet, but it
// does leave adoption.deliveries > adoption.atRunEnd, which the helper reads in the
// same critical section the delivery mutates and refuses to file from. Filing a
// marker whose FiledAt postdates that keystroke would, on a restart, leave only the
// durable marker and the pre-keystroke pane-churn watermark — and the unpaused drain
// would authorize the posting the user vetoed. The helper refuses; the unpaused drain
// stands down on the in-memory deliveries check; a restart that wipes that in-memory
// check wipes the (never-filed) marker too, leaving the pre-#4162 shape the churn
// watermark is designed to cover.
func (m *Manager) fileOwedTaskLifecycle(repoID string, instance *session.Instance) {
	m.installOwedTaskLifecycleNotify(repoID, instance)
	if instance.OwedOnComplete() != nil {
		return
	}
	if instance.FileOwedOnCompleteIfNotDischarged(&session.PendingOnCompleteData{
		TaskID:  instance.TaskID,
		FiledAt: nowFunc(),
	}) {
		m.persistOwedTaskLifecycle(repoID, instance)
	}
}

// dischargeOwedTaskLifecycle settles the obligation durably. It re-resolves the
// session by stable id so a same-titled replacement never loses state it does
// not own, and it is a no-op when the row or the marker is already gone — a
// killed session's row delete IS the discharge, and a delivery can beat a late
// drain to the same decision.
func (m *Manager) dischargeOwedTaskLifecycle(repoID, sessionID, title string) {
	m.mu.Lock()
	inst := m.instances[daemonInstanceKey(repoID, title)]
	m.mu.Unlock()
	if inst == nil || inst.ID != sessionID {
		return
	}
	if inst.OwedOnComplete() == nil {
		return
	}
	inst.SetOwedOnComplete(nil)
	m.persistOwedTaskLifecycle(repoID, inst)
}

// installOwedTaskLifecycleNotify wires the adoption discharge to disk: a
// delivery clears the marker inside NoteAdoptionDelivery's critical section and
// then fires this callback after unlocking, so the user's veto survives a
// restart landing between the two. The callback is in-memory, so it must be
// re-installed on every marked row a generation materializes — filing does it
// for live sessions and armOwedTaskLifecyclesLocked does it for restored ones.
func (m *Manager) installOwedTaskLifecycleNotify(repoID string, instance *session.Instance) {
	instance.SetOwedOnCompleteNotify(func(i *session.Instance) {
		m.persistOwedTaskLifecycle(repoID, i)
	})
}

// persistOwedTaskLifecycle checkpoints a marker change, but only while THIS
// instance still owns its row: a kill plus a same-titled re-create hands the key
// to a different session, and writing the predecessor's snapshot back would
// resurrect it. persistSettlement cannot express that — the registration
// re-check must sit inside the same repo-ordered critical section as the write
// or it reopens exactly the window it exists to close — so this holds the repo
// lock across both and reuses persistSettlement's write and retry bookkeeping.
func (m *Manager) persistOwedTaskLifecycle(repoID string, instance *session.Instance) {
	key := daemonInstanceKey(repoID, instance.Title)
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	m.mu.Lock()
	registered := m.instances[key] == instance
	m.mu.Unlock()
	if !registered {
		return
	}
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	m.publishEvent(agentproto.EventSessionUpdated, data)
	m.recordSettlementWrite(repoID, key, instance, err)
	if err != nil {
		m.warn().Printf("the on_complete obligation for session %q could not be written to disk "+
			"(the daemon retries it on its poll; an unclean exit before it lands would lose it): %v",
			instance.Title, err)
	}
}

// armOwedTaskLifecyclesLocked is the restart half of the contract: every
// restored row still carrying the marker gets its notify re-installed and its
// intent re-parked, so the first unpaused poll tick drains it through
// applyDeferredTaskSessionLifecycle — the same re-validation and the same
// fenced teardown the dropped worker was driving. Runs under m.mu, immediately
// after m.instances is published, from both restoreInstances and refreshLocked.
//
// Three marked-row shapes are NOT parked. A tombstoned row belongs to the kill
// that will finish it — the row delete takes the marker with it. An archived
// row has already discharged it: archive's own persist drops the marker (the
// serialize gate), and only the in-memory copy is left for the lifecycle worker
// to clear once the archive call returns. A refresh inside that window — any
// session lookup refreshes — is the ordinary order of an on_complete=archive
// run, not a fault, so it is settled in memory at INFO (#4853). An inert row
// (never started, or settled startup-unknown) can never reach the drain because
// the poll returns before it; that marker is settled instead, by a detached
// writer because the discharge performs storage I/O and m.mu is held.
func (m *Manager) armOwedTaskLifecyclesLocked() {
	for key, inst := range m.instances {
		marker := inst.OwedOnComplete()
		if marker == nil {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		m.installOwedTaskLifecycleNotify(repoID, inst)
		switch {
		case inst.UserKilled():
			// finishUserKill owns this row now; the delete takes the marker.
		case inst.IsArchived():
			// Checked before the inert case: SetArchived clears started, so an
			// archived row would otherwise read as inert and warn on every
			// archived task run.
			inst.SetOwedOnComplete(nil)
			m.info().Printf("task %s: session %q is archived; its on_complete obligation filed at %s is settled",
				marker.TaskID, inst.Title, marker.FiledAt.Format(time.RFC3339))
		case !inst.Started() || inst.StartupStateUnknown():
			m.warn().Printf("task %s: session %q is owed an on_complete teardown filed at %s, but af cannot confirm its runtime (it never started, or its startup state is unknown), so the teardown will not run; the obligation is dropped and the session left in place — check it and archive or kill it by hand if its work is done",
				marker.TaskID, inst.Title, marker.FiledAt.Format(time.RFC3339))
			m.launchBackgroundMutation(func(<-chan struct{}) {
				m.dischargeOwedTaskLifecycle(repoID, inst.ID, inst.Title)
			})
		default:
			if m.deferredTaskLifecycle == nil {
				m.deferredTaskLifecycle = make(map[string]string)
			}
			m.deferredTaskLifecycle[key] = inst.ID
		}
	}
}
