package daemon

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
)

// rebindRegisteredProject is RebindProject's registry write: a compare-and-set
// against the root the caller last observed when it sent one (#4822), and last
// writer wins when it did not. A refusal on that precondition leaves as a
// projectReboundError, so both transports can tell it apart from every other
// rejection.
func rebindRegisteredProject(id, expectedRoot, path string) (config.Project, error) {
	project, err := config.RebindProjectIfRoot(id, strings.TrimSpace(expectedRoot), path)
	var rebound *config.ProjectReboundError
	if errors.As(err, &rebound) {
		return config.Project{}, &projectReboundError{err: err}
	}
	return project, err
}

// projectReboundError carries config.ProjectReboundError onto the wire: HTTP
// answers it with 409 and apiproto.ErrorCodeProjectRebound, so a client shows a
// definitive "refresh and retry" refusal instead of a generic failure. Nothing
// was written, so it is an ordinary daemon rejection, never a committed outcome.
type projectReboundError struct {
	err error
}

func (e *projectReboundError) Error() string        { return e.err.Error() }
func (e *projectReboundError) Unwrap() error        { return e.err }
func (e *projectReboundError) APIErrorCode() string { return apiproto.ErrorCodeProjectRebound }
func (e *projectReboundError) HTTPStatus() int      { return http.StatusConflict }
