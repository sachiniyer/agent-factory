package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/apiproto"
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
func interpretEnvelopeError(msg, code string) error {
	if code == apiproto.ErrorCodeMutationCommitted {
		return &mutationCommittedError{detail: msg}
	}
	if m := unknownFieldPattern.FindStringSubmatch(msg); m != nil {
		return &VersionSkewError{Field: m[1], Detail: msg}
	}
	return fmt.Errorf("%s", msg)
}

// mutationCommittedError tells mutation callers that the daemon durably wrote
// their change before the reported follow-up failure. Its marker method keeps
// the classification usable through errors.Join and other wrappers.
type mutationCommittedError struct {
	detail string
}

func (e *mutationCommittedError) Error() string           { return e.detail }
func (e *mutationCommittedError) MutationCommitted() bool { return true }

// IsMutationCommitted distinguishes a rejected mutation from a durable one
// whose post-commit work failed. The latter still surfaces as an error, but a
// caller must advance its persistence baseline instead of retrying the write.
func IsMutationCommitted(err error) bool {
	type committed interface {
		MutationCommitted() bool
	}
	var outcome committed
	return errors.As(err, &outcome) && outcome.MutationCommitted()
}

// RouteNotServedError reports that the daemon answered 404 for a /v1 route this
// client called — the route is absent from THAT daemon's table, so the daemon is
// OLDER than this client (or, on a remote target, something other than a daemon
// is answering the URL).
//
// The inference is sound rather than a guess, and it is the reason this is a
// separate type from VersionSkewError. The daemon's rpcHandler answers only 200,
// 400, 405, 413, 500 and 503; a 404 comes from exactly one place, the mux
// catch-all (daemon/httpserver.go), which is reached only by a path no route
// registers. So a 404 on /v1/<Method> means the method is not served, never that
// a handler ran and refused.
//
// It exists because the two are opposite instructions to a caller. An envelope
// error is the daemon's considered answer and is final. A route that is not
// served means NOTHING happened server-side — which is exactly what `af config
// set --daemon-url` must be able to distinguish, because its only other option
// would be writing THIS machine's config file for a change the operator asked to
// make on another one (#3679). Collapsing both into a bare error is what would
// make that fallback look reasonable.
type RouteNotServedError struct {
	// Route is the request path that 404ed, e.g. "/v1/UnsetConfigValue".
	Route string
	// Detail is the peer's verbatim message — the daemon's `unknown route "…"`
	// envelope, or a snippet of whatever non-envelope body a proxy returned.
	Detail string
}

func (e *RouteNotServedError) Error() string {
	return fmt.Sprintf("daemon does not serve %s (it answered 404: %s)", e.Route, e.Detail)
}

// IsRouteNotServed reports whether err (or anything it wraps) is a
// RouteNotServedError — the route is missing from the daemon that answered, as
// opposed to present and refusing.
func IsRouteNotServed(err error) bool {
	var missing *RouteNotServedError
	return errors.As(err, &missing)
}

// notServedDetail renders a 404 body for the refusal message: the daemon's own
// envelope message when it sent one (`unknown route "/v1/…"`), else a snippet of
// whatever a proxy answered with. It is best-effort by construction — the body is
// no longer what DECIDES the classification, only what describes it — so a body
// it cannot parse costs a nicer sentence, never the classification itself.
func notServedDetail(raw []byte) string {
	var env struct {
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return bodySnippet(raw)
}

// bodySnippetLimit caps how much of a non-envelope response body is quoted back.
// A proxy's 404 page is HTML and can be kilobytes; the first line is the part
// that identifies who answered.
const bodySnippetLimit = 200

// bodySnippet renders an unparseable response body for a human-readable error:
// whitespace-collapsed and truncated, so an nginx error page becomes a
// recognizable fragment rather than a wall of markup.
//
// The limit is a BYTE count (it is a cap on message size), but the cut is made
// on a rune boundary. A localized proxy error page is not ASCII, and slicing a
// Go string at an arbitrary byte index happily splits a multi-byte rune, so the
// naive form ends the message in an invalid fragment that renders as U+FFFD.
func bodySnippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if s == "" {
		return "empty response body"
	}
	if len(s) <= bodySnippetLimit {
		return s
	}
	// Walk back to the start of the rune the limit lands inside. A rune is at
	// most 4 bytes, so this steps back 3 times at worst.
	cut := bodySnippetLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
