package daemon

import (
	"errors"
	"net/rpc"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// daemonStoppingErrText is the stable wire prefix for work refused or cut short
// because the daemon is stopping for good: not handing off to an upgrade, and
// not restarting, just going away (#7440). It is deliberately not an admission
// retry. The client must not wait it out, nor re-ensure a daemon behind the
// caller's own stop. The caller retries after the daemon restarts.
const daemonStoppingErrText = "agent-factory daemon is shutting down"

// errDaemonStopping refuses work the stopping daemon will not do, and tells the
// caller what to do instead.
func errDaemonStopping() error {
	return errors.New(daemonStoppingErrText + "; retry after it restarts")
}

// IsDaemonStoppingErr reports whether err is the stopping daemon's refusal or
// abort. It matches the wire prefix, since net/rpc flattens errors to text.
func IsDaemonStoppingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonStoppingErrText)
}

// errDaemonRestarting refuses a request, unrun, because the daemon is
// restarting: whoever stopped it starts its successor, so a client retries
// into that successor (#7474). It is typed, and a client classifies it by its
// code, never by its text.
func errDaemonRestarting() error {
	return &lifecycleRefusalError{refusal: apiproto.LifecycleRefusal{
		Code:    apiproto.ErrorCodeDaemonRestarting,
		Message: "agent-factory daemon is restarting; retry shortly",
	}}
}

// IsDaemonRestartingErr reports whether err is a restarting daemon's refusal,
// on either transport: the daemon's own error, or the one a client decoded
// from the wire. A client retries it into the successor.
func IsDaemonRestartingErr(err error) bool {
	return apiproto.IsLifecycleRefusal(err, apiproto.ErrorCodeDaemonRestarting)
}

// lifecycleRefusalCodes are the lifecycle refusals that cross the control
// socket typed. A code a client does not know stays plain text, which no
// classifier retries: an unknown refusal fails closed.
var lifecycleRefusalCodes = []string{apiproto.ErrorCodeDaemonRestarting}

// lifecycleRefusalError is a lifecycle refusal as the daemon raises it. net/rpc
// keeps only an error's text, so its text leads with the refusal's code, which
// lifecycleRefusalFromRPCError reads back into the typed refusal. HTTP carries
// the code in the envelope instead, and writes the message without it
// (writeHTTPError). Only an unwrapped refusal decodes: a handler that wraps one
// ran, so its outcome is not a refusal a client may retry.
type lifecycleRefusalError struct {
	refusal apiproto.LifecycleRefusal
}

func (e *lifecycleRefusalError) Error() string {
	return lifecycleRefusalWirePrefix(e.refusal.Code) + e.refusal.Message
}

// Unwrap exposes the typed refusal, so the daemon's own classifiers see it.
func (e *lifecycleRefusalError) Unwrap() error { return &e.refusal }

func lifecycleRefusalWirePrefix(code string) string { return "[" + code + "] " }

// bareLifecycleRefusalTexts are the exact texts the admission gate raises its
// lifecycle refusals with.
var bareLifecycleRefusalTexts = []string{errDaemonRestarting().Error()}

// controlSocketErrorText is the text the control socket sends for a handler's
// error (gobServerCodec.WriteResponse). Only the admission gate's own refusal,
// unwrapped, may lead with a lifecycle refusal's code (#7480). Any other error
// that does, such as a wrap of one (`fmt.Errorf("%w; …", refusal)`,
// `errors.Join(refusal, x)`), came from a handler that ran. Its code is
// removed, so the client reads a plain error and never retries the request,
// exactly as HTTP's exact type assertion already answers it.
//
// The strip never empties the text: net/rpc delivers an empty error as a
// success with a zero reply, so an error that was only the code keeps a text
// that still reads as a failure and decodes as nothing.
func controlSocketErrorText(text string) string {
	if slices.Contains(bareLifecycleRefusalTexts, text) {
		return text
	}
	original := text
	for stripped := true; stripped; {
		stripped = false
		for _, code := range lifecycleRefusalCodes {
			if rest, ok := strings.CutPrefix(text, lifecycleRefusalWirePrefix(code)); ok {
				text, stripped = rest, true
			}
		}
	}
	if text == "" && original != "" {
		return "the daemon failed with an error that held only " + strings.TrimSpace(original)
	}
	return text
}

// lifecycleRefusalFromRPCError reads a lifecycle refusal back out of the text
// net/rpc carried, and returns any other error unchanged. The server lets the
// code lead only the gate's own refusal (controlSocketErrorText).
func lifecycleRefusalFromRPCError(err error) error {
	srv, ok := err.(rpc.ServerError)
	if !ok {
		return err
	}
	for _, code := range lifecycleRefusalCodes {
		if msg, ok := strings.CutPrefix(string(srv), lifecycleRefusalWirePrefix(code)); ok {
			return &apiproto.LifecycleRefusal{Code: code, Message: msg}
		}
	}
	return err
}
