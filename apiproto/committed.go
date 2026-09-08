package apiproto

import "errors"

// MutationCommittedError is the marker a transport-specific error implements to
// say its mutation reached durable storage before a later, non-transactional
// follow-up failed — the outcome ErrorCodeMutationCommitted names on the wire.
//
// It lives beside that constant because "committed" is one concept with several
// carriers: the HTTP client rebuilds it from the envelope code, the control
// socket rebuilds it from a gob-encoded MutationOutcome, and the daemon raises
// it directly. Each of those owns its own error TYPE, but they must agree on
// what makes a type committed, so the interface and the predicate below are the
// single definition all of them are checked against.
//
// Implementers should pin the relationship with a compile-time assertion —
//
//	var _ apiproto.MutationCommittedError = (*myError)(nil)
//
// — because the alternative failure is silent: a misspelled or re-typed method
// still compiles, IsMutationCommitted simply stops matching, and a durable
// mutation is reported as a clean failure that a caller may safely retry.
type MutationCommittedError interface {
	// MutationCommitted reports that the mutation landed. Implementations
	// return true unconditionally; the type's presence in the error chain is
	// what carries the signal.
	MutationCommitted() bool
}

// IsMutationCommitted distinguishes a rejected mutation from a durable one whose
// post-commit work failed. The latter still surfaces as an error, but a caller
// must advance its persistence baseline instead of retrying the write.
//
// It matches through errors.Join and other wrappers, so an error may acquire
// context on its way up without losing the classification.
func IsMutationCommitted(err error) bool {
	var outcome MutationCommittedError
	return errors.As(err, &outcome) && outcome.MutationCommitted()
}
