package task

import (
	"errors"
	"fmt"
	"testing"

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
