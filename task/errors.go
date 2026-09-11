package task

import (
	"errors"
	"fmt"
)

type taskNotFoundError struct {
	id string
}

func (e *taskNotFoundError) Error() string {
	return fmt.Sprintf("task with id %q not found", e.id)
}

func newTaskNotFoundError(id string) error {
	return &taskNotFoundError{id: id}
}

// IsTaskNotFound reports a definitive read-under-lock absence. Parse, lock, and
// storage errors are deliberately excluded: callers may retire obligations only
// when the store was read successfully and the requested row was not present.
func IsTaskNotFound(err error) bool {
	var target *taskNotFoundError
	return errors.As(err, &target)
}
