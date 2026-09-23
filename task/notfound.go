package task

import (
	"errors"
	"fmt"
	"strings"
)

// NotFoundError reports that no task with ID exists in the store. Its text is
// the store's long-standing `task with id "<id>" not found` message, which the
// CLI prints and the daemon returns verbatim, so the type changes nothing a
// user or script sees.
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("task with id %q not found", e.ID)
}

// IsNotFound reports whether err positively says the task with id does not
// exist: a NotFoundError for that ID, or — once the daemon's RPC has flattened
// the error to text — a message ending in exactly that error's text. Any other
// failure, including a not-found for a different ID, is not a positive answer.
// The TUI relies on this to drop an unsaved draft only when its task is known
// to be gone (#4798), so a false negative merely keeps the draft retryable.
func IsNotFound(err error, id string) bool {
	if err == nil || id == "" {
		return false
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return nf.ID == id
	}
	return strings.HasSuffix(err.Error(), (&NotFoundError{ID: id}).Error())
}
