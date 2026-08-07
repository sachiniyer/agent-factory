package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// A COMMITTED mutation is one whose side effect landed and whose FOLLOW-UP
// failed. "failed" and "failed after committing" demand opposite handling: only
// the first is safe to retry blindly, and the second must never be presented as
// if nothing happened. Everything that makes that distinction survive — the
// marker, its constructor, the API code, the RPC-handler rule — lives here so a
// new mutation path adopts the whole thing rather than a piece of it (#3029).

// mutationCommittedError preserves the otherwise-ambiguous outcome of a
// durable mutation whose non-transactional follow-up failed. HTTP exposes its
// code additively; net/rpc carries the server-owned prefix that the control
// client narrowly restores to the same three-valued marker.
type mutationCommittedError struct {
	err error
}

const (
	taskAddCommittedErrorPrefix    = "task add committed, but failed to reload task schedules:"
	taskUpdateCommittedErrorPrefix = "task update committed, but failed to reload task schedules:"
	taskRemoveCommittedErrorPrefix = "task removal committed, but failed to reload task schedules:"
)

func (e *mutationCommittedError) Error() string           { return e.err.Error() }
func (e *mutationCommittedError) Unwrap() error           { return e.err }
func (e *mutationCommittedError) MutationCommitted() bool { return true }

// committedFailure marks a failure whose SIDE EFFECTS ALREADY LANDED.
//
// It exists so the marker is one call rather than a three-line literal each site
// re-derives. Three outcomes must be distinguishable on every mutating path, not
// two: succeeded, failed with nothing committed, and failed with the work already
// done. Only the middle is safe to retry blindly — a retry of the third is not
// recovery, it re-runs a partially applied change.
//
// The rule for choosing it: ask what survives if the caller does nothing. If the
// answer is "the operation happened", this is the constructor to use, and the
// event announcing what happened belongs immediately before it — every other
// client learns the truth from that event, not from the error.
func committedFailure(format string, args ...any) error {
	return &mutationCommittedError{err: fmt.Errorf(format, args...)}
}

func (e *mutationCommittedError) APIErrorCode() string {
	return apiproto.ErrorCodeMutationCommitted
}

// commitWarning classifies a mutation's error for a control-socket handler.
//
// A COMMITTED failure is not a failed call: the side effect landed and only the
// follow-up failed. Returned as an error it reaches clients as an
// rpc.ServerError with the marker stripped — indistinguishable from "nothing
// happened" — so every such handler answers OK and carries the text as a
// warning instead. Named rather than copied so a new mutation handler cannot
// half-adopt it (#3029).
//
// ok=false means a genuine failure the handler must return unchanged.
func commitWarning(err error) (warning string, ok bool) {
	switch {
	case err == nil:
		return "", true
	case isMutationCommitted(err):
		return err.Error(), true
	default:
		return "", false
	}
}

func isMutationCommitted(err error) bool {
	type committed interface {
		MutationCommitted() bool
	}
	var outcome committed
	return errors.As(err, &outcome) && outcome.MutationCommitted()
}
