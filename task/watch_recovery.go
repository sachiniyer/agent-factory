package task

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RecoverStaleWatcherStatus retires a stale terminal last_run_status once a
// watch process has proven healthy for a full crash window (#5441): the
// daemon's recovery timer calls it with status "started" and expected set to
// the last_run_status the watcher observed when it was armed. The write is a
// compare-and-swap — the status is replaced only when the persisted value is
// still that same terminal outcome ("stopped", or an "errored"/"errored:*"
// breaker or arming-refusal summary per isTerminalWatcherStatus). A newer
// value on disk — a fresh arming refusal, a delivered "sent", a rate-drop or
// usage-limit record — is never touched, and an expected value that was not
// terminal to begin with (a fresh task, a delivered one) earns no write
// either. Check and write are one critical section under the task-file
// lock — the same discipline RecordWatchRateDrops uses — so anything
// committed in the window can never be clobbered. LastRunAt is preserved: it
// tracks event deliveries, not supervision changes.
//
// Returns true only when a terminal status was actually replaced.
func RecoverStaleWatcherStatus(taskID, expected, status string) (bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return false, err
	}
	if !isTerminalWatcherStatus(expected) {
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
			if tasks[i].LastRunStatus != expected {
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
