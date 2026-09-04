package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// How the watcher supervisor is reconciled against tasks.json, and — the whole
// point of splitting this out of watcher.go — HOW MUCH of it one reconcile is
// allowed to touch.
//
// A watcher that returned from run() is in a TERMINAL state: the crash-loop
// breaker tripped, or the script exited 0 and said it was done. Both promise the
// operator a durable stop. That promise used to be decorative (#3837): every
// task write ran a reconcile over the WHOLE store, a terminal watcher failed the
// `!w.finished()` freshness test, and it was dropped and replaced by a fresh one
// with its failure count and backoff chain reset. Editing an unrelated task
// therefore performed the "reload" the log line describes as a deliberate
// gesture — four trips and four resurrections in six hours on the dev box, with
// last_run_status flapping in and out of the failure the TUI renders.
//
// So a reconcile now carries a scope. A task write reconciles the ONE task it
// wrote; the full re-arm is reserved for the gestures that mean it — the
// ReloadTasks poke, daemon start (armTaskAutomation), and an explicit
// enable/disable of the task itself.

// watchScope names how much of the watcher set one reconcile may touch.
//
// The zero value is the full re-arm, so the gestures that mean "re-read
// everything" say so by passing nothing.
type watchScope struct {
	// taskID limits the reconcile to one task; empty means every task.
	taskID string
	// rearm marks a write that is itself a re-arm gesture for taskID — an
	// explicit enable/disable — and so may restart that task's terminal
	// watcher even though nothing about its process definition changed.
	rearm bool
}

// everyWatchTask is the full re-arm: the ReloadTasks poke and daemon start.
func everyWatchTask() watchScope { return watchScope{} }

// oneWatchTask scopes a reconcile to the task a write named. rearm is true when
// the write was an explicit enable/disable of that task.
func oneWatchTask(taskID string, rearm bool) watchScope {
	return watchScope{taskID: taskID, rearm: rearm}
}

func (sc watchScope) full() bool { return sc.taskID == "" }

// covers reports whether this reconcile is entitled to start, restart or stop
// the given task's watcher at all. Everything outside the scope is left exactly
// as it is — running watchers keep running, terminal ones stay terminal.
func (sc watchScope) covers(taskID string) bool {
	return sc.full() || sc.taskID == taskID
}

// rearmsTerminal reports whether this reconcile should restart a watcher that
// has already stopped for good. Only the deliberate gestures do: a full re-arm,
// or a write that named this task AND was an enable/disable of it. A signature
// change restarts a terminal watcher regardless — that is a different process to
// run, not a re-arm of the one that failed.
func (sc watchScope) rearmsTerminal(taskID string) bool {
	return sc.full() || (sc.rearm && sc.taskID == taskID)
}

// Reload re-reads tasks.json and re-arms every watch task: enabled watch tasks
// without a live watcher are started — including ones whose script previously
// exited or crash-looped, so this is the re-arm path a stopped watcher waits
// for. Watchers whose task was disabled or removed are stopped, and a watcher
// whose process-defining fields changed is restarted with the new config.
// Delivery-only fields (prompt, target_session, program) are not part of that
// signature: deliverWatchEvent re-loads the task per event, so editing them
// takes effect without killing a long-lived watch script.
func (s *watcherSupervisor) Reload() error {
	return s.ReloadScoped(everyWatchTask())
}

// ReloadScoped is Reload limited to what scope allows it to touch.
func (s *watcherSupervisor) ReloadScoped(scope watchScope) error {
	tasks, err := s.loadTasks()
	if err != nil {
		return err
	}
	return s.reconcile(tasks, tasks, scope)
}

// reconcile brings the running watchers in line with armed, within scope, using
// allTasks only as the authoritative inventory for orphan-queue cleanup. A
// lifecycle-unsafe task can therefore remain unarmed without being mistaken for
// deleted.
func (s *watcherSupervisor) reconcile(armed, allTasks []task.Task, scope watchScope) error {
	desired := make(map[string]task.Task)
	for _, t := range armed {
		if !t.Enabled || !t.IsWatch() {
			continue
		}
		// The ID flows into the stderr log path; reject hand-edited IDs the
		// same way RunTask does before any filesystem path is built.
		if err := task.ValidateTaskID(t.ID); err != nil {
			log.WarningLog.Printf("not watching task with invalid id %q: %v", t.ID, err)
			continue
		}
		// Duplicate IDs in a hand-edited store: watch the FIRST occurrence, which
		// is what the cron scheduler already does (#855). A map assignment quietly
		// kept the LAST instead, so the two subsystems disagreed about which row a
		// duplicated ID meant — and nothing anywhere said a row had been skipped.
		if _, dup := desired[t.ID]; dup {
			log.WarningLog.Printf("duplicate task ID %q in tasks.json, watching only its first occurrence", t.ID)
			continue
		}
		desired[t.ID] = t
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fmt.Errorf("watch task supervisor is shutting down")
	}

	var stale []*taskWatcher
	for id, w := range s.watchers {
		if !scope.covers(id) {
			continue
		}
		t, ok := desired[id]
		if ok && watcherSignature(t) == w.sig && !(w.finished() && scope.rearmsTerminal(id)) {
			continue
		}
		stale = append(stale, w)
		delete(s.watchers, id)
	}
	// Wait for stale watchers to die before starting replacements so two
	// processes for the same task never overlap. Bounded by stopGrace via
	// the per-watcher SIGKILL escalation.
	stopWatchers(stale)

	for id, t := range desired {
		if !scope.covers(id) {
			continue
		}
		// A watcher still on the books is either live or deliberately terminal;
		// the loop above already dropped every one this reconcile may replace.
		if _, running := s.watchers[id]; running {
			continue
		}
		w := s.newTaskWatcher(t)
		s.watchers[id] = w
		go w.run()
	}
	// Only a full re-arm has observed the whole store, and the latch means
	// exactly that: before it, an empty map is "not observed yet", never "this
	// watch task is not armed" (#3623). A scoped write that landed in the
	// startup window before armTaskAutomation must not claim otherwise.
	if scope.full() {
		s.armed = true
	}

	// Queue files for tasks that no longer exist at all are removed — a
	// deleted task's backlog must not replay into a recreated namesake. A
	// merely-disabled task keeps its backlog for re-enable (#1129). Runs after
	// stopWatchers so no stale drainer is mid-replay on a file being removed.
	s.cleanOrphanQueues(allTasks, scope)
	return nil
}

// cleanOrphanQueues removes event-queue files whose task ID is absent from
// tasks.json entirely.
//
// Scoped the same way the watchers are, and for the same reason: outside the
// scope this reconcile did not stop anything, so a still-running watcher would
// otherwise have the file its drainer is replaying deleted underneath it. The
// removal a scoped write cares about — its own task's — is always in scope,
// and the full re-arm still sweeps everything.
func (s *watcherSupervisor) cleanOrphanQueues(tasks []task.Task, scope watchScope) {
	dir, err := s.queueDir()
	if err != nil {
		return
	}
	known := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		known[t.ID] = struct{}{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(name, ".jsonl"), ".cursor")
		if id == name { // neither suffix matched
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if !scope.covers(id) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove orphan event-queue file %s: %v", name, err)
		}
	}
}
