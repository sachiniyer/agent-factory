package task

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Session-backed task-run statuses belong here with the row mutations that
// classify them. Both values describe an active run owned by LastRunSessionID:
// parked means its prompt is queued at a usage-limit fence, while started means
// the prompt reached its runtime.
const (
	RunStatusStarted     = "started"
	RunStatusLimitParked = "parked: usage limit"
)

// UpdateTaskStatus updates only the scheduler-owned last-run fields. Unlike
// UpdateTask, it does not re-validate other fields (notably Program), so legacy
// tasks can still receive status changes. A nil lastRunAt preserves the
// timestamp, session identity, and sequence; watcher supervision uses that form
// so it cannot detach a live run from the token its outcome needs. A non-nil
// timestamp starts a non-session-backed status and clears that token, while
// retaining the sequence as the allocator's durable high-water mark.
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
// calls it while the new session is still hidden behind Manager.mu. The manager
// assigned sequence orders different run IDs by admission rather than by a
// timestamp or whichever delivery caller happens to resume first. A lower or
// equal sequence is a delayed writer and is left untouched.
func BeginTaskRun(taskID, runID string, runSequence uint64, runAt time.Time, lastRunStatus string) (Task, bool, error) {
	if runID == "" || runSequence == 0 {
		return Task{}, false, fmt.Errorf("task run id and sequence are required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if runSequence <= t.LastRunSequence {
			return false
		}
		t.LastRunAt = &runAt
		t.LastRunStatus = lastRunStatus
		t.LastRunSessionID = runID
		t.LastRunSequence = runSequence
		return true
	})
}

// UpdateTaskRunStart is the post-RPC repair for BeginTaskRun. A greater manager
// sequence safely replaces even an identified prior run after the manager's
// earlier task-store write failed; the same or a newer run is left untouched.
// Thus a delayed repair cannot reopen its own terminal outcome or overwrite a
// successor.
func UpdateTaskRunStart(taskID, runID string, runSequence uint64, runAt time.Time, lastRunStatus string) (Task, bool, error) {
	return BeginTaskRun(taskID, runID, runSequence, runAt, lastRunStatus)
}

// UpdateTaskRunOutcome changes only the current session-backed run, and only
// while its delivery status is active (started, parked at a usage limit, or
// blank after a temporary not-armed marker was cleared). Matching the stable
// session ID makes equal timestamps and clock corrections irrelevant; checking
// the status preserves later watcher supervision evidence such as "stopped" or
// "errored".
func UpdateTaskRunOutcome(taskID, runID, lastRunStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != runID || !sessionRunStatusActive(t.LastRunStatus) {
			return false
		}
		t.LastRunStatus = lastRunStatus
		return true
	})
}

// AdvanceTaskRunStatus changes the status of the identified active run without
// changing its identity or display timestamp. Limit resume uses it to turn the
// parked delivery into started after its queued prompt lands. A later watcher
// supervision status is not the expected status and therefore wins the race.
func AdvanceTaskRunStatus(taskID, runID, fromStatus, toStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != runID || t.LastRunStatus != fromStatus {
			return false
		}
		t.LastRunStatus = toStatus
		return true
	})
}

func sessionRunStatusActive(status string) bool {
	// Arming supervision writes and later clears "errored: not armed" without
	// changing the identified run. The exact nonempty session-ID match in the
	// caller proves a blank row still belongs to that run; no terminal watcher
	// status is admitted here.
	return status == "" || status == RunStatusStarted || status == RunStatusLimitParked
}

// ClaimUnidentifiedTaskRunOutcome is the compatibility writer for a legacy row
// or a start whose manager-ordered publication failed. The caller first proves
// the candidate can own the row; this function then compares its exact identity,
// sequence, status, and timestamp under the file lock before installing the
// durable identity and outcome. desiredRunAt is the historical stored timestamp
// for a legacy row and the session's creation timestamp for a current failure.
func ClaimUnidentifiedTaskRunOutcome(
	taskID, runID string,
	expectedRunAt *time.Time,
	expectedStatus string,
	expectedSessionID string,
	expectedSequence uint64,
	desiredRunAt time.Time,
	desiredSequence uint64,
	lastRunStatus string,
) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.LastRunSessionID != expectedSessionID || t.LastRunSequence != expectedSequence ||
			t.LastRunStatus != expectedStatus ||
			!sameOptionalTime(t.LastRunAt, expectedRunAt) {
			return false
		}
		t.LastRunAt = &desiredRunAt
		t.LastRunStatus = lastRunStatus
		t.LastRunSessionID = runID
		t.LastRunSequence = desiredSequence
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
