package apiclient

import (
	"context"
	"errors"
)

// malformedResponseError reports a response this client could not decode. It
// arrives AFTER the daemon — or something standing in front of it — answered, so
// for a mutation it proves nothing about whether the handler ran. Its text is
// the underlying decode error, unchanged.
type malformedResponseError struct{ err error }

func (e *malformedResponseError) Error() string { return e.err.Error() }
func (e *malformedResponseError) Unwrap() error { return e.err }

// IsTransportErrorNotSent reports whether err carries a TransportError whose
// request provably never reached the daemon (TransportError.NotSent). It is the
// one transport failure a mutation may re-send: nothing was received, so nothing
// can have committed.
func IsTransportErrorNotSent(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.NotSent
}

// IsMutationOutcomeUncertain reports whether a failed mutation may nonetheless
// have landed, so the caller must neither re-send it nor present it as refused.
// It is the Go twin of the web client's isMutationOutcomeUncertain
// (web/src/api.ts): only a definitive answer from the daemon permits treating the
// mutation as not applied.
//
// Uncertain means any of:
//   - the daemon said it committed before a follow-up failed
//     (apiproto.IsMutationCommitted — the web helper counts this too);
//   - a transport failure after a connection was obtained: the request may be
//     on the wire and the reply lost (#4820);
//   - a response this client could not verify or decode: an unmarked proxy 404,
//     a malformed envelope;
//   - a cancelled or expired context outside a TransportError: the caller gave
//     up waiting, which says nothing about what the daemon did.
//
// A TransportError that was never sent is NOT uncertain: no request byte left
// the process. Neither is an envelope error, which is the daemon's own refusal.
func IsMutationOutcomeUncertain(err error) bool {
	if err == nil {
		return false
	}
	if IsMutationCommitted(err) {
		return true
	}
	var te *TransportError
	if errors.As(err, &te) {
		return !te.NotSent
	}
	var unconfirmed *UnconfirmedHTTPResponseError
	var malformed *malformedResponseError
	return errors.As(err, &unconfirmed) || errors.As(err, &malformed) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
