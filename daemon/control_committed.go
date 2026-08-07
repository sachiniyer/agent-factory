package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// Everything that makes a COMMITTED mutation distinguishable from a clean
// failure lives here: the marker, its constructor, the API code, and the two
// halves of the control-socket envelope. One home, so a new write path meets the
// whole rule rather than a fragment of it (#3036).

func isMutationCommitted(err error) bool {
	type committed interface {
		MutationCommitted() bool
	}
	var outcome committed
	return errors.As(err, &outcome) && outcome.MutationCommitted()
}
func (e *mutationCommittedError) Error() string           { return e.err.Error() }
func (e *mutationCommittedError) Unwrap() error           { return e.err }
func (e *mutationCommittedError) MutationCommitted() bool { return true }
func (e *mutationCommittedError) APIErrorCode() string {
	return apiproto.ErrorCodeMutationCommitted
}

// mutationCommittedError preserves the otherwise-ambiguous outcome of a
// durable mutation whose non-transactional follow-up failed. HTTP exposes its
// code additively; net/rpc carries the server-owned prefix that the control
// client narrowly restores to the same three-valued marker.
type mutationCommittedError struct {
	err error
}

// record classifies a mutation's error for a control-socket handler and fills
// the response envelope, returning false for a genuine failure the handler must
// return unchanged. The ONLY place a handler decides what committed means.
func (o *MutationOutcome) record(err error) (ok bool) {
	switch {
	case err == nil:
		return true
	case isMutationCommitted(err):
		o.Code = apiproto.ErrorCodeMutationCommitted
		o.Warning = err.Error()
		return true
	default:
		return false
	}
}
