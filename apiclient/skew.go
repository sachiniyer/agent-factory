package apiclient

import (
	"fmt"
	"regexp"
)

// unknownFieldPattern matches the daemon's strict-decoder rejection. It requires
// encoding/json's exact wording — `json: unknown field "tab_id"` — rather than a
// loose "unknown field", so the only thing that can match is a real JSON request
// decode. Two near-misses this deliberately excludes: the config loader's strict
// pass is TOML and reports `unknown key`, and a daemon error that happened to
// quote user text must never be re-read as a skew and send someone to restart a
// perfectly healthy daemon.
var unknownFieldPattern = regexp.MustCompile(`json: unknown field "([^"]+)"`)

// VersionSkewError reports that the daemon rejected a request field this client
// sent, which means the daemon is OLDER than this client.
//
// The inference is sound rather than a guess. Every request this client sends
// carries agentproto.ClientVersionHeader, and a daemon that understands that
// header never strict-decodes an af client's body — it ignores fields it does
// not know. So a daemon that still answers "unknown field" is necessarily one
// built before that behavior existed, and the field it choked on is one this
// client's version added. Nothing else in the API produces this message shape.
//
// This exists because the raw message is a dead end for a user: a TUI reporting
// `malformed JSON request body: json: unknown field "tab_id"` names a field the
// user never typed, on a request they never wrote, and says nothing about what to
// do. The actual remedy — restart the daemon so it matches the binary on disk —
// is not derivable from it.
//
// Why the client must self-diagnose at all: the daemon is upgraded independently
// of its clients (#960), and the party that rejects the field is by definition a
// daemon that predates the tolerant decoder. Making the decoder forward-compatible
// therefore cannot help anyone already running an older daemon — only the client
// noticing and saying so can.
type VersionSkewError struct {
	// Field is the request field the daemon refused, e.g. "tab_id".
	Field string
	// Detail is the daemon's verbatim message, preserved for the log.
	Detail string
}

func (e *VersionSkewError) Error() string {
	return fmt.Sprintf(
		"daemon is out of date and rejected the %q field this client sent — restart it with `af daemon restart` (daemon said: %s)",
		e.Field, e.Detail)
}

// interpretEnvelopeError converts a daemon envelope message into an error,
// upgrading a provable version skew into an actionable VersionSkewError and
// passing everything else through verbatim so existing callers that match on
// daemon message text are unaffected.
func interpretEnvelopeError(msg string) error {
	if m := unknownFieldPattern.FindStringSubmatch(msg); m != nil {
		return &VersionSkewError{Field: m[1], Detail: msg}
	}
	return fmt.Errorf("%s", msg)
}
