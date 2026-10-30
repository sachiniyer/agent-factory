package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session"
)

// streamLivenessErrorBodyLimit is the byte budget the daemon's JSON failure
// envelope must fit within for a non-101 WS handshake refusal to reach the
// client intact. coder/websocket.Dial buffers only the first 1024 bytes of a
// failed upgrade response (its dial.go LimitReader(resp.Body, 1024) on the error
// path) and closes the rest, so a larger envelope is truncated before the client
// reads it: the JSON unmarshal in apiclient.notServedDetail then fails on the
// incomplete body and falls back to a 200-byte snippet that can be the session
// title alone — dropping the liveness status and the restore off-ramp this gate
// exists to surface. The send-prompt path is a regular HTTP RPC with no such cap,
// so promptTargetLivenessError stays verbatim; only the WS stream path is bounded
// here. Session titles are uncapped by validateTitleShapeLocked (it checks shape,
// not length), and the Archived refusal embeds the title twice (the %q label and
// the shellsuggest restore command), so a long enough title overflows the buffer
// on its own.
const streamLivenessErrorBodyLimit = 1024

// streamTargetLivenessError mirrors promptTargetLivenessError's refusal for the
// WS attach path, where the whole JSON envelope must fit the 1 KiB the dial
// client reads (streamLivenessErrorBodyLimit). A title short enough that the
// verbatim refusal fits (the common case) gets it byte-identical. Otherwise the
// refusal drops the title echo (the %q label — decoration) and keeps what the
// reader must act on: the liveness status and the restore command with the REAL
// title, since restore resolves by exact title and a shortened one would name a
// session that does not exist. If even that command cannot fit, the refusal says
// so and gives the generic form rather than presenting a runnable-looking
// command with the wrong argument. Every size decision is a handful of linear
// passes: a title longer than the budget is never rendered at all.
func streamTargetLivenessError(title string, liveness session.Liveness) error {
	if len(title) <= streamLivenessErrorBodyLimit {
		full := promptTargetLivenessError(title, liveness)
		if full == nil {
			return nil
		}
		if streamRefusalEnvelopeSize(full.Error()) <= streamLivenessErrorBodyLimit {
			return full
		}
	} else if promptTargetLivenessError("", liveness) == nil {
		return nil
	}
	const tooLong = "(its title is too long to repeat here)"
	switch liveness {
	case session.LiveLost:
		return errors.New("target session is Lost; prompt not delivered; recover it first " + tooLong)
	case session.LiveDead:
		return errors.New("target session is Dead; prompt not delivered; recover it first " + tooLong)
	}
	// Archived: the only other refusing state.
	if len(title) <= streamLivenessErrorBodyLimit {
		cmd := shellsuggest.PositionalCommand("af", []string{"sessions", "restore"}, title)
		msg := fmt.Sprintf("target session is Archived; prompt not delivered; restore it first (%s)", cmd)
		if streamRefusalEnvelopeSize(msg) <= streamLivenessErrorBodyLimit {
			return errors.New(msg)
		}
	}
	return errors.New("target session is Archived; prompt not delivered; restore it first with `af sessions restore -- <title>` " + tooLong)
}

// streamRefusalEnvelopeSize returns the byte length of the failure envelope the
// daemon's writeHTTPError serializes for msg on this path — the same shape
// (apiproto.Failure, daemon_rejected=true, MarshalIndented, newline-terminated)
// the WS dial client reads from the truncated body — so the truncation decision
// matches the wire.
func streamRefusalEnvelopeSize(msg string) int {
	env := apiproto.Failure(msg)
	if env.Error != nil {
		env.Error.DaemonRejected = true
	}
	b, err := apiproto.MarshalIndented(env)
	if err != nil {
		// MarshalIndent never errors for this structure; fall back to a
		// conservative estimate so the gate still refuses safely.
		return len(msg) + 80
	}
	// WriteEnvelope terminates with a newline; the dial buffer counts it.
	return len(b) + 1
}
