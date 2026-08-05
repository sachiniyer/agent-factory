package daemon

import (
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
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

// runEndedIntoIdle reports whether this tick is the moment a task run finished
// with its session sitting idle and healthy.
//
// Both halves are load-bearing:
//
//   - the run marker went true→false on THIS tick. taskRunActive flips once and
//     permanently, so this can fire at most once per session — a session cannot be
//     archived twice, and a session a user later adopts and works in is never
//     revisited. That is the difference between an edge and a standing predicate,
//     and it is why this is not a sweep: "task session whose run has ended" stays
//     true forever, including for the session someone is typing into right now.
//
//   - the session settled into LiveReady. taskRunActive also clears on paths that
//     are NOT a completed run: CommitArchive ends it (already archived — nothing to
//     do), and a startup that settles terminal-unknown ends it without the session
//     ever having run. That last one matters most: the daemon deliberately RETAINS
//     an uncertain create's record so an operator can inspect the workspace it may
//     have left behind (keepUncertainCreate), and reaping it here would destroy
//     exactly what that retention exists to preserve.
//
// The idle edge itself is session.runEndsOnIdleEdge — a transition INTO Ready from
// somewhere else — so pairing "the run ended" with "we are now Ready" identifies it
// without this package having to re-derive the edge.
func runEndedIntoIdle(instance *session.Instance, taskRunWasActive bool) bool {
	if !taskRunWasActive || instance.TaskRunActive() {
		return false
	}
	return instance.GetLiveness() == session.LiveReady
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
	title := instance.Title
	verb, err := m.taskSessionLifecycle(repoID, taskID)
	if err != nil {
		// An unreadable or unscopable task store is not permission to tear a
		// session down. Keep it — the conservative outcome, and the same one an
		// older daemon produced — and say so once, on the tick that could not
		// decide. A later run's completion asks again.
		log.WarningLog.Printf("could not read the session lifecycle for task %s (session %q): %v; leaving the session in place",
			taskID, title, err)
		return
	}
	if verb == task.OnCompleteKeep {
		return
	}
	go m.runTaskSessionLifecycle(repoID, title, taskID, verb)
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

// runTaskSessionLifecycle performs the teardown on its own goroutine.
//
// It reuses ArchiveSession/KillSession rather than reaching for the primitives
// underneath, so a task-driven teardown is the SAME operation a user's
// `af sessions archive` is: the same killsInFlight + op-lock serialization
// (#2779), the same refusal when a task still targets the session, the same
// events. A policy that tore down sessions through a private path would be a
// second lifecycle implementation, and the two would drift.
func (m *Manager) runTaskSessionLifecycle(repoID, title, taskID, verb string) {
	var err error
	switch verb {
	case task.OnCompleteArchive:
		_, _, err = m.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	case task.OnCompleteKill:
		_, err = m.KillSession(KillSessionRequest{Title: title, RepoID: repoID})
	default:
		// Unreachable: ValidateTrigger refuses an unknown verb on write, and
		// SessionLifecycle canonicalizes. Log rather than guess — picking a verb
		// here would be inventing destructive intent from a value nothing accepted.
		log.WarningLog.Printf("task %s declares an unknown on_complete %q; leaving session %q in place", taskID, verb, title)
		return
	}
	if err != nil {
		// Failing to reap is not a failure of the RUN, which already succeeded, so
		// this never touches the task's last-run status. The session stays where it
		// is and stays visible, which is the recoverable outcome; a user can finish
		// the teardown by hand.
		log.WarningLog.Printf("task %s: could not %s session %q after its run finished: %v", taskID, verb, title, err)
		return
	}
	log.InfoLog.Printf("task %s: %sd session %q after its run finished (on_complete=%s)", taskID, verb, title, verb)
}
