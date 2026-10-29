package daemon

import (
	"github.com/sachiniyer/agent-factory/apiproto"
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

// streamLivenessTitleTruncationMarker signals that a stream refusal trimmed an
// over-long title to fit the WS dial buffer (streamLivenessErrorBodyLimit), so a
// reader does not mistake a truncated title for the real one.
const streamLivenessTitleTruncationMarker = "…"

// streamTargetLivenessError mirrors promptTargetLivenessError's refusal but caps
// the title so the daemon's JSON failure envelope fits the WS dial's 1 KiB body
// buffer (streamLivenessErrorBodyLimit). For any title short enough that the
// verbatim refusal already fits (the common case), the message is byte-identical
// to the send-prompt path; only a title long enough to overflow the buffer is
// trimmed on a rune boundary with a visible marker, keeping the liveness status
// and the restore off-ramp in the part the client can read. The full title still
// resolves the session on the daemon side — this trims only the human-readable
// refusal, not the resolution.
func streamTargetLivenessError(title string, liveness session.Liveness) error {
	full := promptTargetLivenessError(title, liveness)
	if full == nil {
		return nil
	}
	if streamRefusalEnvelopeSize(full.Error()) <= streamLivenessErrorBodyLimit {
		return full
	}
	// The title drives the overflow (it appears in the %q label and the
	// shellsuggest restore command). Shrink it on a rune boundary with a marker
	// until the rendered envelope fits the buffer.
	r := []rune(title)
	for max := len(r); max > 0; max -= 8 {
		capped := string(r[:max]) + streamLivenessTitleTruncationMarker
		if trimmed := promptTargetLivenessError(capped, liveness); trimmed != nil &&
			streamRefusalEnvelopeSize(trimmed.Error()) <= streamLivenessErrorBodyLimit {
			return trimmed
		}
	}
	// A title so pathological that even a one-rune prefix overflows (should not
	// happen): return the verbatim refusal so the gate still refuses; the client
	// falls back to the handshake status, which still names the failure.
	return full
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
