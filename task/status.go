package task

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// UpdateTaskStatus updates only the LastRunAt and LastRunStatus fields of the
// task with the given ID. Unlike UpdateTask, it does not re-validate other
// fields (notably Program), so pre-existing tasks whose Program value would
// fail current enum validation can still have their run status bumped by the
// scheduler and TUI dispatch paths. Returns an error if no task with the given
// ID exists.
//
// A nil lastRunAt means "leave LastRunAt untouched" — only LastRunStatus is
// written. Callers that record a supervision-status change (not an event
// delivery) pass nil so a concurrent writer's newer LastRunAt is never reverted
// by a value the caller read outside the file lock (#1215).
// It returns the record as committed, identified against the file the write
// produced. Returning it rather than only an error is what stops the next caller
// that PUBLISHES a status change from announcing the copy it walked in with:
// that copy was identified against the pre-write bytes, so it names a version of
// the store that this very call retired (#3684 review). Callers that only need
// the error discard it.
func UpdateTaskStatus(taskID string, lastRunAt *time.Time, lastRunStatus string) (Task, error) {
	updated, _, err := updateTaskStatus(taskID, lastRunAt, lastRunStatus, taskStatusAlways)
	return updated, err
}

// UpdateTaskStatusIfLastRunAt updates LastRunStatus only when the stored
// LastRunAt still identifies expectedRunAt. It is the completion/interruption
// writer for a session-backed run: another run may have updated the task row
// while this session was alive, and an older outcome must not overwrite the
// newer run's status. The comparison and write share the task-file lock.
//
// Reports false with no error when the row does not identify that run (normally
// because a newer one owns it). LastRunAt is left unchanged whichever way the
// comparison goes.
func UpdateTaskStatusIfLastRunAt(taskID string, expectedRunAt time.Time, lastRunStatus string) (Task, bool, error) {
	return updateTaskStatus(taskID, &expectedRunAt, lastRunStatus, taskStatusAtExactRun)
}

// UpdateTaskRunStart records a delivered run only if no equal or later run is
// already represented. Equality is deliberately a refusal: recovery may have
// recorded the outcome after the session became visible but before the delivery
// caller returned, and that terminal outcome must not be changed back to
// "started".
func UpdateTaskRunStart(taskID string, runAt time.Time, lastRunStatus string) (Task, bool, error) {
	return updateTaskStatus(taskID, &runAt, lastRunStatus, taskStatusBeforeRun)
}

// UpdateTaskRunOutcome records a session-backed run's terminal outcome while
// its identity is still current. A missing or older row is claimed too: the
// session and its run identity are durable before the delivery caller writes
// "started", so recovery can legitimately win that race. A later run is never
// overwritten.
func UpdateTaskRunOutcome(taskID string, runAt time.Time, lastRunStatus string) (Task, bool, error) {
	return updateTaskStatus(taskID, &runAt, lastRunStatus, taskStatusAtOrBeforeRun)
}

type taskStatusMode uint8

const (
	taskStatusAlways taskStatusMode = iota
	taskStatusAtExactRun
	taskStatusBeforeRun
	taskStatusAtOrBeforeRun
)

func updateTaskStatus(taskID string, runAt *time.Time, lastRunStatus string, mode taskStatusMode) (Task, bool, error) {
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

		found := false
		row := -1
		for i := range tasks {
			if tasks[i].ID == taskID {
				found = true
				if !taskStatusMayApply(tasks[i].LastRunAt, runAt, mode) {
					return nil
				}
				// nil means "preserve the on-disk LastRunAt": a status-only
				// update must not clobber a newer event-delivery timestamp that
				// a concurrent writer committed while this caller held a stale
				// copy (#1215).
				if mode != taskStatusAtExactRun && runAt != nil {
					tasks[i].LastRunAt = runAt
				}
				tasks[i].LastRunStatus = lastRunStatus
				row = i
				applied = true
				break
			}
		}

		if !found {
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

func taskStatusMayApply(stored, runAt *time.Time, mode taskStatusMode) bool {
	switch mode {
	case taskStatusAlways:
		return true
	case taskStatusAtExactRun:
		return stored != nil && runAt != nil && stored.Equal(*runAt)
	case taskStatusBeforeRun:
		return runAt != nil && (stored == nil || stored.Before(*runAt))
	case taskStatusAtOrBeforeRun:
		return runAt != nil && (stored == nil || !stored.After(*runAt))
	default:
		return false
	}
}
