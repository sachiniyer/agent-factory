package task

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RecoverStaleWatcherStatus retires a stale terminal last_run_status once a
// watch process has proven healthy for a full crash window (#5441): the
// daemon's recovery timer calls it with status "started" and armed set to the
// task record the watcher observed when it was armed. The write is a
// compare-and-swap over the row's SUPERVISION EVIDENCE, not just the status
// text — the status is replaced only when nothing that bears on the watcher's
// health has moved since arming (see sameWatcherEvidence): the status and
// last-run stamp, the rate-drop counter, enabled, and the three fields that
// identify the running process. Comparing that evidence is the row-scoped
// currentness check a store-wide generation cannot give: a file generation
// moves on EVERY task's write, while this question is only "did anything
// that matters rewrite THIS row since arming". A fresh arming refusal, a
// delivered "sent", a rate-drop or usage-limit record, or an edit that
// changes the process is newer evidence and the swap refuses. A delivery-only
// edit (prompt, program, schedule metadata, audit) leaves the same process
// running and carries no health evidence, so it must not strand a healthy
// watcher on its stale terminal status (#5650). A status value that was never terminal to
// begin with (a fresh task, a delivered one) earns no write at all. Check and
// write are one critical section under the task-file lock — the same
// discipline RecordWatchRateDrops uses — so anything committed in the window
// can never be clobbered. LastRunAt is preserved: it tracks event deliveries,
// not supervision changes.
//
// Returns true only when the armed row was still on disk and was replaced.
func RecoverStaleWatcherStatus(taskID string, armed Task, status string) (bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return false, err
	}
	expected := armed
	expected.stripDerived()
	if !isTerminalWatcherStatus(expected.LastRunStatus) {
		return false, nil
	}
	path, err := getTasksPathFn()
	if err != nil {
		return false, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return false, err
	}

	recovered := false
	lockErr := config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}
		found := false
		for i := range tasks {
			if tasks[i].ID != taskID {
				continue
			}
			found = true
			stored := tasks[i]
			stored.stripDerived()
			if !sameWatcherEvidence(stored, expected) {
				return nil
			}
			tasks[i].LastRunStatus = status
			break
		}
		if !found {
			return fmt.Errorf("task with id %q not found", taskID)
		}
		if _, err := writeTasks(tasks); err != nil {
			return err
		}
		recovered = true
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return recovered, nil
}

// sameWatcherEvidence reports whether nothing that bears on a running
// watcher's health differs between two records of the same task: the status
// and its last-run stamp, the cumulative rate-drop count, whether the task is
// enabled, and WatchCmd/ProjectPath/Name — the fields that identify the
// process (daemon's watcherSignature). Everything else is delivery
// configuration the running process does not depend on. Times compare with
// Equal so zone representation cannot manufacture a difference.
func sameWatcherEvidence(a, b Task) bool {
	if a.LastRunStatus != b.LastRunStatus || a.DroppedEvents != b.DroppedEvents || a.Enabled != b.Enabled ||
		a.WatchCmd != b.WatchCmd || a.ProjectPath != b.ProjectPath || a.Name != b.Name {
		return false
	}
	if (a.LastRunAt == nil) != (b.LastRunAt == nil) {
		return false
	}
	return a.LastRunAt == nil || a.LastRunAt.Equal(*b.LastRunAt)
}
