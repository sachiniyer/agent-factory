package task

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// #4798: the TUI drops an unsaved draft only on a positive not-found for its
// own task, so IsNotFound must answer yes to exactly that and nothing else.
func TestIsNotFound(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"typed":                {&NotFoundError{ID: "a1"}, true},
		"typed and wrapped":    {fmt.Errorf("update: %w", &NotFoundError{ID: "a1"}), true},
		"daemon message":       {errors.New(`task with id "a1" not found`), true},
		"prefixed message":     {errors.New(`failed to update task: task with id "a1" not found`), true},
		"typed, other task":    {&NotFoundError{ID: "b2"}, false},
		"message, other task":  {errors.New(`task with id "b2" not found`), false},
		"message, id prefix":   {errors.New(`task with id "a1x" not found`), false},
		"generic failure":      {errors.New("The daemon refused this save."), false},
		"not-found mid-string": {errors.New(`task with id "a1" not found; retry later`), false},
		"nil":                  {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsNotFound(tc.err, "a1"))
		})
	}
}

// The typed error keeps the store's long-standing message byte for byte: the
// CLI prints it and the daemon returns it verbatim.
func TestNotFoundErrorKeepsTheStoreMessage(t *testing.T) {
	assert.Equal(t, `task with id "a1" not found`, (&NotFoundError{ID: "a1"}).Error())
}

// UpdateTask reports a missing task as a *NotFoundError, so an in-process
// caller gets the positive answer without parsing text.
func TestUpdateTaskMissingIsNotFoundError(t *testing.T) {
	setupTestTasks(t, []Task{{ID: "other1", Name: "other", Prompt: "p", CronExpr: "0 0 * * *"}})
	enabled := false
	_, err := UpdateTask("missing1", TaskUpdate{Enabled: &enabled}, ProjectExpectation{})
	var nf *NotFoundError
	assert.ErrorAs(t, err, &nf)
	assert.True(t, IsNotFound(err, "missing1"))
}

// Every store entry point that looks a task up by ID reports a missing one as
// the same *NotFoundError, so an in-process caller can tell "the task is gone"
// from a store failure with errors.As rather than by reading text. The daemon's
// re-verification watch retires on exactly that answer (#7042), and a store
// error of any other kind must keep it retrying.
func TestStoreEntryPointsReportMissingTaskAsNotFoundError(t *testing.T) {
	const missing = "missing1"
	now := time.Now()
	for name, call := range map[string]func() error{
		"UpdateTaskStatus": func() error {
			_, err := UpdateTaskStatus(missing, &now, "sent")
			return err
		},
		"UpdateTaskStatusIf": func() error {
			_, _, err := UpdateTaskStatusIf(missing, nil, "sent", func(Task) bool { return true })
			return err
		},
		"UpdateTaskStatusForTarget": func() error {
			_, err := UpdateTaskStatusForTarget(missing, &now, "sent", "root")
			return err
		},
		"RecordRunStatus": func() error {
			_, _, err := RecordRunStatus(missing, RunKey{}, &now, "sent")
			return err
		},
		"GetTask": func() error {
			_, err := GetTask(missing)
			return err
		},
		"RemoveTask": func() error {
			return RemoveTask(missing, ProjectExpectation{})
		},
		"RecordTaskTrigger": func() error {
			return RecordTaskTrigger(missing, ActorCLI)
		},
		"RecordWatchRateDrops": func() error {
			_, err := RecordWatchRateDrops(missing, 1, now)
			return err
		},
		"RecoverStaleWatcherStatus": func() error {
			_, err := RecoverStaleWatcherStatus(missing, Task{ID: missing, LastRunStatus: "stopped"}, "watching")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			setupTestTasks(t, []Task{{ID: "other1", Name: "other", Prompt: "p", CronExpr: "0 0 * * *"}})
			err := call()
			var nf *NotFoundError
			if assert.ErrorAs(t, err, &nf) {
				assert.Equal(t, missing, nf.ID)
			}
			assert.EqualError(t, err, `task with id "missing1" not found`,
				"the typed error must keep the store's message byte for byte")
		})
	}
}
