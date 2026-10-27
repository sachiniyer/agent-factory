package task

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// RecoverStaleWatcherStatus retires a stale terminal last_run_status once a
// watch process has proven healthy for a full crash window (#5441): the
// daemon's recovery timer calls it with status "started" and armed set to the
// task record the watcher observed when it was armed. The write is a
// compare-and-swap over the whole ROW, not just the status text — the status
// is replaced only when the persisted record is still exactly the one the
// watcher armed over. Comparing persisted content is the row-scoped
// currentness check a store-wide generation cannot give: a file generation
// moves on EVERY task's write, while this question is only "did anything
// rewrite THIS row since arming". Anything that did — a fresh arming refusal,
// a delivered "sent", a rate-drop or usage-limit record, an edit — is newer
// evidence and the swap refuses. A status value that was never terminal to
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
			if !samePersistedTask(stored, expected) {
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

// samePersistedTask compares two task records on their persisted content
// alone. Both must already be stripDerived-normalized; comparing the
// marshaled form rather than the structs keeps time zones and other
// parse-internal differences from manufacturing inequality.
func samePersistedTask(a, b Task) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}
