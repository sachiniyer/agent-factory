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
// so it cannot detach a live run from the token its outcome needs. Every applied
// write advances LastRunRevision, including status-only supervision. Session-
// backed writers do not advance that barrier: generation, stable session ID,
// and sequence order their changes without making concurrent admissions reject
// one another. A non-nil timestamp starts a non-session-backed status and clears
// the session token, while retaining the sequence as the allocator's durable
// high-water mark.
func UpdateTaskStatus(taskID string, lastRunAt *time.Time, lastRunStatus string) (Task, error) {
	updated, _, err := updateTaskStatus(taskID, "", false, lastRunAt, lastRunStatus)
	return updated, err
}

// UpdateTaskStatusForGeneration updates scheduler-owned fields only while the
// task ID still names the incarnation the caller loaded. It is for deliveries
// and supervisors that may finish after remove+re-add reused the user-facing ID.
// A generation mismatch is a clean refusal (applied=false), not a storage error.
func UpdateTaskStatusForGeneration(
	taskID, expectedGenerationID string,
	lastRunAt *time.Time,
	lastRunStatus string,
) (Task, bool, error) {
	return updateTaskStatus(taskID, expectedGenerationID, true, lastRunAt, lastRunStatus)
}

func updateTaskStatus(
	taskID, expectedGenerationID string,
	requireGeneration bool,
	lastRunAt *time.Time,
	lastRunStatus string,
) (Task, bool, error) {
	var revisionErr error
	updated, applied, err := mutateTaskStatus(taskID, func(t *Task) bool {
		if requireGeneration && t.GenerationID != expectedGenerationID {
			return false
		}
		if t.LastRunRevision == ^uint64(0) {
			revisionErr = fmt.Errorf("task last-run revision exhausted")
			return false
		}
		t.LastRunRevision++
		if lastRunAt != nil {
			t.LastRunAt = lastRunAt
			t.LastRunSessionID = ""
		}
		t.LastRunStatus = lastRunStatus
		return true
	})
	if err == nil && revisionErr != nil {
		return Task{}, false, revisionErr
	}
	return updated, applied, err
}

// BeginTaskRun authoritatively publishes a session-per-run delivery. The daemon
// calls it while the new session is still hidden behind Manager.mu. The manager
// assigned sequence orders different run IDs by admission rather than by a
// timestamp or whichever delivery caller happens to resume first. GenerationID
// binds the write to the task incarnation, while expectedRevision proves no
// task-wide status observation landed after admission. Session publications do
// not advance that revision; their sequence decides which admitted run is
// newest. A mismatched proof or a lower/equal sequence leaves the row untouched.
func BeginTaskRun(
	taskID, taskGenerationID, runID string,
	runSequence, expectedRevision uint64,
	runAt time.Time,
	lastRunStatus string,
) (Task, bool, error) {
	if runID == "" || runSequence == 0 {
		return Task{}, false, fmt.Errorf("task run id and sequence are required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.GenerationID != taskGenerationID || t.LastRunRevision != expectedRevision ||
			runSequence <= t.LastRunSequence {
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
// earlier task-store write failed; it carries the same generation and revision
// proofs as the in-manager publication. Thus a delayed repair cannot reopen its
// own terminal outcome, overwrite watcher supervision, or claim a replacement
// task incarnation.
func UpdateTaskRunStart(
	taskID, taskGenerationID, runID string,
	runSequence, expectedRevision uint64,
	runAt time.Time,
	lastRunStatus string,
) (Task, bool, error) {
	return BeginTaskRun(taskID, taskGenerationID, runID, runSequence, expectedRevision, runAt, lastRunStatus)
}

// UpdateTaskRunOutcome changes only the current session-backed run, and only
// while its delivery status is active (started, parked at a usage limit, or
// blank after a temporary not-armed marker was cleared). Matching the task
// generation and stable session ID makes ID reuse, equal timestamps, and clock
// corrections irrelevant; checking the status preserves later watcher
// supervision evidence such as "stopped" or "errored".
func UpdateTaskRunOutcome(taskID, taskGenerationID, runID, lastRunStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.GenerationID != taskGenerationID || t.LastRunSessionID != runID ||
			!sessionRunStatusActive(t.LastRunStatus) {
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
func AdvanceTaskRunStatus(taskID, taskGenerationID, runID, fromStatus, toStatus string) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.GenerationID != taskGenerationID || t.LastRunSessionID != runID ||
			t.LastRunStatus != fromStatus {
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
// the candidate can own the row; this function then compares its exact task
// generation, revision, run identity, sequence, status, and timestamp under the
// file lock before installing the durable identity and outcome. desiredRunAt is
// the historical stored timestamp for a legacy row and the session's creation
// timestamp for a current failure.
func ClaimUnidentifiedTaskRunOutcome(
	taskID, runID string,
	expectedRunAt *time.Time,
	expectedStatus string,
	expectedSessionID string,
	expectedSequence uint64,
	expectedRevision uint64,
	expectedGenerationID string,
	desiredRunAt time.Time,
	desiredSequence uint64,
	lastRunStatus string,
) (Task, bool, error) {
	if runID == "" {
		return Task{}, false, fmt.Errorf("task run id is required")
	}
	return mutateTaskStatus(taskID, func(t *Task) bool {
		if t.GenerationID != expectedGenerationID ||
			t.LastRunSessionID != expectedSessionID || t.LastRunSequence != expectedSequence ||
			t.LastRunRevision != expectedRevision ||
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
