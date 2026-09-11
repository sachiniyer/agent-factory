package task

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// UpdateTaskStatus updates only the scheduler-owned last-run fields. Unlike
// UpdateTask, it does not re-validate other fields (notably Program), so legacy
// tasks can still receive status changes. A nil lastRunAt preserves both the
// timestamp and its session identity; watcher supervision uses that form so it
// cannot detach a live run from the token its outcome needs. A non-nil timestamp
// starts a non-session-backed status and clears that token.
func UpdateTaskStatus(taskID string, lastRunAt *time.Time, lastRunStatus string) (Task, error) {
	updated, _, err := mutateTaskStatus(taskID, func(t *Task) bool {
		if lastRunAt != nil {
			t.LastRunAt = lastRunAt
			t.LastRunSessionID = ""
		}
		t.LastRunStatus = lastRunStatus
		return true
	})
	return updated, err
}

// BeginTaskRun authoritatively publishes a session-per-run delivery. The daemon
// calls it while the new session is still hidden behind Manager.mu, so different
// run IDs are ordered by manager publication rather than by timestamps or by
// whichever delivery caller happens to resume first.
func BeginTaskRun(taskID, runID string, runAt time.Time, lastRunStatus string) (Task, error) {
	if runID == "" {
		return Task{}, fmt.Errorf("task run id is required")
	}
	updated, _, err := mutateTaskStatus(taskID, func(t *Task) bool {
		t.LastRunAt = &runAt
		t.LastRunStatus = lastRunStatus
		t.LastRunSessionID = runID
		return true
	})
	return updated, err
}

// UpdateTaskRunStart is the post-RPC repair for BeginTaskRun. It fills an
// unidentified row when the manager's earlier task-store write failed, but
// refuses any identified row — including the same run after recovery recorded
// its outcome, and a different run published in the meantime.
func UpdateTaskRunStart(taskID, runID string, runAt time.Time, lastRunStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != "" {
			return false
		}
		t.LastRunAt = &runAt
		t.LastRunStatus = lastRunStatus
		t.LastRunSessionID = runID
		return true
	})
}

// UpdateTaskRunOutcome changes only the current session-backed run, and only
// while its delivery status is still "started". Matching the stable session ID
// makes equal timestamps and clock corrections irrelevant; checking the status
// preserves later watcher supervision evidence such as "stopped" or "errored".
func UpdateTaskRunOutcome(taskID, runID, lastRunStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != runID || t.LastRunStatus != "started" {
			return false
		}
		t.LastRunStatus = lastRunStatus
		return true
	})
}

// ClaimUnidentifiedTaskRunOutcome is the compatibility writer for a task row
// with no run token. The caller first establishes whether that legacy/missed
// start can belong to the session; this function then compares the exact row
// snapshot under the file lock before installing the durable identity and
// outcome. desiredRunAt is the historical stored timestamp for a legacy row and
// the session's creation timestamp for a current start-publication failure.
func ClaimUnidentifiedTaskRunOutcome(
	taskID, runID string,
	expectedRunAt *time.Time,
	expectedStatus string,
	desiredRunAt time.Time,
	lastRunStatus string,
) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != "" || t.LastRunStatus != expectedStatus ||
			!sameOptionalTime(t.LastRunAt, expectedRunAt) {
			return false
		}
		t.LastRunAt = &desiredRunAt
		t.LastRunStatus = lastRunStatus
		t.LastRunSessionID = runID
		return true
	})
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func mutateTaskStatus(taskID string, mutate func(*Task) bool) (Task, bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return Task{}, false, err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, false, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, false, err
	}
	var updated Task
	applied := false
	lockErr := config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}

		row := -1
		for i := range tasks {
			if tasks[i].ID != taskID {
				continue
			}
			row = i
			if !mutate(&tasks[i]) {
				return nil
			}
			applied = true
			break
		}
		if row < 0 {
			return fmt.Errorf("task with id %q not found", taskID)
		}
		if !applied {
			return nil
		}

		generation, err := writeTasks(tasks)
		if err != nil {
			return err
		}
		updated = stampRowIdentity(tasks, generation)[row]
		return nil
	})
	if lockErr != nil {
		return Task{}, false, lockErr
	}
	return updated, applied, nil
}
