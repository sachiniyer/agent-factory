// Package credscrub holds the credential-shape patterns Agent Factory scrubs
// out of text, and the single Scrub that applies them.
//
// It exists because two sinks need the identical policy and used to have
// different ones (#2884): the bug-report bundle, which is built to be shared,
// and agent-factory.log, whose writer previously redacted only `access_token`.
// The same GitHub PAT was therefore removed from a bundle and written in
// cleartext to disk. Anything that learns a new credential shape must teach it
// here, so a sink cannot fall behind again.
//
// The patterns are deliberately narrow. A broad "any long opaque string" rule
// would also destroy the git SHAs, session ids and tmux names a triager needs,
// so this is best-effort on high-confidence shapes rather than a guarantee; the
// bug-report bundle still tells the user to review before sharing.
package credscrub

import (
	"regexp"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// Markers replacing redacted content. SecretMarker replaces a substring a
// pattern flagged as a credential inside otherwise-kept text; RedactedMarker is
// the whole-field marker its callers write, named here only so the
// already-redacted check below can recognize both.
const (
	SecretMarker   = "[redacted-secret]"
	RedactedMarker = "[redacted]"
)

// shapePatterns are targeted, high-confidence credential shapes scrubbed
// wherever they appear.
var shapePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                     // OpenAI / Anthropic-style keys (incl. sk-ant-…)
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                                // GitHub PAT / OAuth / server / refresh tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                              // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                              // Slack tokens
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                          // AWS access key id
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                     // Google API key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`), // JWT (header.payload.signature)
}

// keyValueSecret matches a `<credential-key> = <value>` / `<key>: <value>`
// assignment and redacts only the value, preserving the key so triage can see
// *that* a credential is configured without leaking it. The key half tolerates
// a prefix (github_token, x-api-key, client_secret) and optional quotes. The
// value half recognizes TOML/JSON-style double-quoted strings, TOML literal
// single-quoted strings, and bare token-like values.
//
// THE BARE CLASS MUST NOT EXCLUDE `]`. It used to, which meant a bare value
// stopped BEFORE a `]` instead of at a real terminator — so the captured text
// was not the value, only a prefix of it. Everything downstream inherited that
// lie: `api_key=[redacted-secret]actualcredential` captured just
// `[redacted-secret`, which looks exactly like a marker this code wrote, and the
// credential rode out untouched behind it. The bug was never in the comparison,
// so no guard on top of the capture could fix it.
//
// The value now ends only at a genuine terminator — whitespace, a quote, `,`,
// `}`, or end of text — so what the regex hands back IS the whole bare value,
// and comparing it to a marker is a real comparison. Values carrying structural
// characters are covered by the quoted alternatives, which consume their own
// delimiters. Dropping `]` also errs toward MORE redaction (a `]` adjacent to a
// bare value is absorbed rather than left behind), which is the safe direction.
// credentialKeyPattern is the key half shared by keyValueSecret and
// strandedAfterMarker, so the two cannot recognize different key sets.
const credentialKeyPattern = `["']?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?["']?\s*[:=]\s*`

var keyValueSecret = regexp.MustCompile(
	`(?i)(` + credentialKeyPattern + `)(?:"(?:\\.|[^"\\\r\n])*"|'[^'\r\n]*'|[^\s"',}]{6,})`)

// keyedSchemeSecret recognizes the original form that the historical
// keyValueSecret -> strandedAfterMarker sequence scrubbed in two mutations.
// Finding both words at once lets callers plan against untouched input without
// enumerating auth scheme names.
var keyedSchemeSecret = regexp.MustCompile(
	`(?i)(` + credentialKeyPattern + `)([^\s"',}]{6,})[ \t]+[A-Za-z0-9._~+/=-]{8,}`)

// strandedAfterMarker removes a credential left stranded BEHIND a marker.
//
// keyValueSecret consumes only the first whitespace-delimited word of a value,
// so `auth: <scheme> <token>` redacts the scheme and leaves the token in the
// clear — behind a marker that makes the line read as though it were scrubbed.
// authScheme handles the two schemes worth naming, but this shape arrives two
// other ways it cannot cover:
//
//   - ANY other scheme word regenerates it (`auth: CustomScheme <token>`), and
//     enumerating scheme names is the losing game this file keeps re-learning.
//   - Lines ALREADY on disk carry it. The log is written scrubbed and the bug
//     report re-bundles that tail, so a line persisted before the ordering fix
//     still reads `auth: [redacted-secret] <token>` — and by then the scheme
//     word is gone, so no amount of scheme matching can recover it.
//
// Keyed on the marker, so it fires only where a credential assignment was
// already redacted and opaque token text follows.
//
// The length floor is 8, matching authScheme rather than being chosen
// independently: authScheme treats `Bearer <8 chars>` as sensitive, so a token
// the old writer persisted as `auth: [redacted-secret] <8 chars>` has to be
// recoverable too. A recovery pass stricter than the pass it recovers for leaves
// exactly the tokens the other one would have caught.
//
// The separator is `[ \t]+`, NOT `\s+`: this runs over multi-line log and config
// blobs, and `\s` matches newlines, so a marker ending one line would consume the
// start of the next unrelated line and silently delete it.
//
// Over-redaction within a line is the safe direction, per the policy above — a
// long path following a redacted value is absorbed rather than left behind.
var strandedAfterMarker = regexp.MustCompile(
	`(?i)(` + credentialKeyPattern + regexp.QuoteMeta(SecretMarker) + `)[ \t]+[A-Za-z0-9._~+/=-]{8,}`)

// authScheme matches an HTTP auth scheme together with its credential, as one
// unit. It must be DISCOVERED on the original text alongside keyValueSecret:
// applying the key/value mutation first would consume only `Bearer` on
// `auth: Bearer <token>` and leave the credential standing behind a marker.
//
// `Authorization: Bearer <token>` happens to survive either order, because the
// key half requires the key to END at `auth` and so never matches
// "Authorization" — which is exactly why testing only that spelling hid the bug.
//
// Only the two real HTTP schemes, not a bare "token", which would eat ordinary
// log prose.
//
// The separator is `[ \t]+`, NOT `\s+`, for the same reason
// strandedAfterMarker's is: Scrub runs over genuine multi-line text blobs from
// the bug-report bundle (the whole config.toml and the daemon log tail), and
// `\s` matches newlines, so a line ending in bare `bearer`/`basic` would cross
// the newline and consume the leading token-run of the next unrelated line —
// on the config path, the TOML key the prior comment documented, and on the
// log path, the timestamp prefix of the next line. A scheme/token separator in
// HTTP is a single SP or HTAB (RFC 7230); `[ \t]+` matches every real
// `Bearer <token>` / `Basic <token>` and nothing across a line boundary.
var authScheme = regexp.MustCompile(`(?i)\b(?:bearer|basic)[ \t]+[A-Za-z0-9._~+/=-]{8,}`)

// privateKeyBlock matches a PEM private-key block in its entirety.
var privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// Scrub replaces every credential shape recognized here with a marker. It is
// idempotent: re-scrubbing text this package already scrubbed returns it
// unchanged, which matters because the bug report scrubs the same text more than
// once by design, and now also scrubs a log that was scrubbed on the way to disk.
func Scrub(s string) string {
	return redactspan.Apply(s, Redactions(s), SecretMarker)
}

// Redactions returns every credential interval recognized in the untouched
// input. Assignment keys are deliberately excluded: they are bounded,
// non-user-authored triage context, while their values are sensitive. Callers
// can combine these intervals with their own privacy matches without one pass
// destroying another's evidence.
func Redactions(s string) []redactspan.Span {
	spans := regexpSpans(nil, s, privateKeyBlock, SecretMarker, 0)
	spans = appendKeyedSchemeSpans(spans, s)
	spans = regexpSpans(spans, s, authScheme, SecretMarker, 2)
	spans = appendKeyValueSpans(spans, s)
	spans = appendStrandedSpans(spans, s)
	for _, re := range shapePatterns {
		spans = regexpSpans(spans, s, re, SecretMarker, 5)
	}
	return spans
}

func appendKeyedSchemeSpans(spans []redactspan.Span, s string) []redactspan.Span {
	for _, loc := range keyedSchemeSecret.FindAllStringSubmatchIndex(s, -1) {
		if len(loc) >= 6 && loc[3] < loc[1] {
			scheme := s[loc[4]:loc[5]]
			// The whole-field marker is not evidence of the historical stranded
			// credential shape. Preserve the established exception: only the
			// credential marker triggers recovery of a following token.
			if scheme == RedactedMarker {
				continue
			}
			spans = append(spans, redactspan.Span{
				Start: loc[3], End: loc[1], Replacement: SecretMarker, Priority: 1,
			})
		}
	}
	return spans
}

func regexpSpans(spans []redactspan.Span, s string, re *regexp.Regexp, replacement string, priority int) []redactspan.Span {
	for _, loc := range re.FindAllStringIndex(s, -1) {
		spans = append(spans, redactspan.Span{
			Start: loc[0], End: loc[1], Replacement: replacement, Priority: priority,
		})
	}
	return spans
}

func appendKeyValueSpans(spans []redactspan.Span, s string) []redactspan.Span {
	for _, loc := range keyValueSecret.FindAllStringSubmatchIndex(s, -1) {
		if len(loc) < 4 || loc[2] < 0 {
			continue
		}
		start, end := loc[3], loc[1]
		value := s[start:end]
		// A value an earlier pass already redacted must survive untouched. Scrub is
		// applied more than once to the same text by design — per section, again over
		// the assembled text/JSON, and again on each component the issue draft inlines
		// — so it has to be idempotent. It was not: re-scrubbing a marker re-wrapped
		// it and grew a bracket per pass, and a real bundle shipped 28
		// `[redacted-secret]]`.
		//
		// This skip is only safe because `value` is the COMPLETE value; see
		// markerValues for why, and keyValueSecret for the boundary that makes it true.
		if isMarker(value) {
			continue
		}
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			spans = append(spans, redactspan.Span{
				Start: start, End: end, Replacement: `"` + SecretMarker + `"`, Priority: 3,
			})
			continue
		}
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			spans = append(spans, redactspan.Span{
				Start: start, End: end, Replacement: `'` + SecretMarker + `'`, Priority: 3,
			})
			continue
		}
		spans = append(spans, redactspan.Span{
			Start: start, End: end, Replacement: SecretMarker, Priority: 3,
		})
	}
	return spans
}

func appendStrandedSpans(spans []redactspan.Span, s string) []redactspan.Span {
	for _, loc := range strandedAfterMarker.FindAllStringSubmatchIndex(s, -1) {
		if len(loc) >= 4 && loc[3] < loc[1] {
			spans = append(spans, redactspan.Span{
				Start: loc[3], End: loc[1], Replacement: "", Priority: 4,
			})
		}
	}
	return spans
}

// markerValues are the EXACT, COMPLETE value forms this package and its callers
// emit, and nothing else. isMarker is a fast-path AROUND the scrub, and a
// fast-path around a redactor is sound only if it recognizes precisely what that
// redactor produces — anything looser is a way for a real credential to reach a
// public bundle unscrubbed.
//
// Every entry is a whole value, which is what makes the comparison sound. That
// is a property of keyValueSecret, not of this map: each alternative in its value
// half ends at a genuine terminator (see the regex comment), so the captured text
// is the entire value —
//
//	bare      `[redacted-secret]`   ends at whitespace/quote/`,`/`}`/EOS
//	bare      `[redacted]`          ditto
//	quoted    `"[redacted-secret]"` the alternative consumes both quotes
//	quoted    `'[redacted-secret]'` ditto
//	quoted    `"[redacted]"`        ditto
//	quoted    `'[redacted]'`        ditto
//
// — so a value that merely BEGINS with a marker (`[redacted-secret]hunter2`,
// `"[redacted-secret]hunter2"`) is captured in full, matches no entry here, and
// takes the normal redacting path. It cannot reach the unchanged path.
//
// Derived from the marker constants so they cannot drift if a marker is reworded.
var markerValues = map[string]bool{
	SecretMarker:               true,
	RedactedMarker:             true,
	`"` + SecretMarker + `"`:   true,
	`'` + SecretMarker + `'`:   true,
	`"` + RedactedMarker + `"`: true,
	`'` + RedactedMarker + `'`: true,
}

// isMarker reports whether value is EXACTLY a marker an earlier scrub pass
// wrote, so re-scrubbing it would only re-wrap it. Exact match against a
// COMPLETE value — never a prefix, never a substring, and never a truncated
// capture: a value this package did not write must take the normal path.
func isMarker(value string) bool {
	return markerValues[value]
}
