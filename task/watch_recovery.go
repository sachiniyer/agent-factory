package task

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RecoverStaleWatcherStatus retires a stale terminal last_run_status once a
// watch process has proven healthy for a full crash window (#5441): the
// daemon's recovery timer calls it with status "started" when a run has been
// continuously alive for watcherCrashWindow. The write is conditional — only
// a persisted watcher terminal outcome ("stopped", or any "errored:*" breaker
// or arming-refusal summary per isTerminalWatcherStatus) is replaced; a
// non-terminal value already on disk ("sent", a rate-drop or usage-limit
// record, or "") is newer evidence and is left untouched. Check and write are
// one critical section under the task-file lock — the same discipline
// RecordWatchRateDrops uses — so a delivery landing in the window can never
// be clobbered. LastRunAt is preserved: it tracks event deliveries, not
// supervision changes.
//
// Returns true only when a terminal status was actually replaced.
func RecoverStaleWatcherStatus(taskID, status string) (bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return false, err
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
			if !isTerminalWatcherStatus(tasks[i].LastRunStatus) {
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
