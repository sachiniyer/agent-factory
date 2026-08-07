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

// MutationOutcome is the control socket's answer to a question its transport
// could not previously express: did the side effect land?
//
// A mutating operation has THREE outcomes — succeeded, failed with nothing
// committed, and failed AFTER something irreversible landed — and only the third
// is unsafe to retry. The HTTP API already carries that distinction structurally
// (apiproto.ErrorCodeMutationCommitted, reconstituted in apiclient/skew.go), but
// the control socket is net/rpc, which reduces any concrete error to
// rpc.ServerError and keeps only its string. Rebuilding the marker from that
// string is what classifyTaskMutationRPCError did, and why it could only ever
// cover the three task RPCs whose message prefixes were fixed.
//
// Embedding it in the RESPONSE puts the answer somewhere the transport cannot
// drop: gob encodes struct fields, so no prefix matching, no per-method casing,
// and a new mutating RPC inherits the channel by embedding rather than by
// remembering (#3036).
//
// VALUE TYPES ONLY — never pointers. gob elides zero values (see
// control_expect_gob_test.go and #1700), so a *bool would arrive nil at false
// and be indistinguishable from "never set", silently turning a committed
// outcome back into a clean failure: exactly the bug this exists to prevent.
type MutationOutcome struct {
	// Code carries apiproto.ErrorCodeMutationCommitted when the mutation
	// committed, and is empty otherwise. It SHARES the HTTP envelope's code
	// rather than paralleling it with a second boolean: one vocabulary means a
	// client asks both transports the same question, and a new signal would be
	// one more thing that can disagree.
	Code string `json:"code,omitempty"`
	// Warning is why the follow-up failed, when Code says it committed.
	Warning string `json:"warning,omitempty"`
}

// CommittedOutcome reports whether the mutation committed and why its follow-up
// failed. Exported because BOTH clients read it — daemon's control client and
// apiclient — generically, off the embedded struct, so a response type opts in
// by embedding rather than by each client growing a per-method branch.
func (o MutationOutcome) CommittedOutcome() (committed bool, warning string) {
	if o.Code == apiproto.ErrorCodeMutationCommitted {
		return true, o.Warning
	}
	// Version skew: a pre-#3036 daemon sends `warning` with no `code`, and that
	// field lands here on both transports (gob matches the promoted name; the
	// json tag flattens). `warning` only ever meant "durable, post-commit step
	// failed", so a code-less warning IS a committed outcome. Delete this branch
	// once the oldest supported daemon emits the code.
	if o.Code == "" && o.Warning != "" {
		return true, o.Warning
	}
	return false, ""
}

// record mirrors the envelope into the flat legacy field so BOTH client
// generations see the committed outcome. It shadows the promoted method, so a
// handler that writes the envelope gets the legacy wire for free.
func (r *ArchiveSessionResponse) record(err error) bool {
	ok := r.MutationOutcome.record(err)
	r.Warning = r.MutationOutcome.Warning
	return ok
}

// CommittedOutcome also consults the flat field, which is where an OLDER
// daemon's warning lands once the outer name takes precedence.
func (r ArchiveSessionResponse) CommittedOutcome() (committed bool, warning string) {
	if committed, warning := r.MutationOutcome.CommittedOutcome(); committed {
		return true, warning
	}
	if r.Warning != "" {
		return true, r.Warning
	}
	return false, ""
}

func (r *DeleteProjectResponse) record(err error) bool {
	ok := r.MutationOutcome.record(err)
	r.Warning = r.MutationOutcome.Warning
	return ok
}

func (r DeleteProjectResponse) CommittedOutcome() (committed bool, warning string) {
	if committed, warning := r.MutationOutcome.CommittedOutcome(); committed {
		return true, warning
	}
	if r.Warning != "" {
		return true, r.Warning
	}
	return false, ""
}
