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
	"github.com/sachiniyer/agent-factory/internal/redactx"
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
//
// It runs over multi-line log and config blobs (the whole config.toml via
// bugreport.collectConfig and the daemon log tail via bugreport.scrubLog —
// see authScheme below for the same rule), and `\s` matches newlines, so the
// pre-narrowing `\s*` separator reached across a line boundary and redacted
// the leading run of the next, unrelated line — the exact cross-newline
// failure mode authScheme's and strandedAfterMarker's separators were both
// corrected away from. The split below keeps that correction for the
// bare-log-line shape and lets the cross-line widening reach only the three
// serializations that legitimately place a credential VALUE on a LATER line
// after a credential KEY: JSON (a symmetrically quoted key followed by `:`),
// an indented YAML continuation (the value's later line is indented), and a
// YAML flow mapping (a `{`/`,` introducer immediately before the key, since
// indentation is not significant inside the braces).
//
// The pattern is four complete alternatives, each carrying its OWN delimiter
// and separator-after, because the cross-line widening is gated to the JSON,
// indented-continuation, YAML-flow, and YAML explicit-key shapes and must
// not leak into the bare-log-line form.
//
//  1. JSON form — `"[key]"` + `(?:[ \t]*(?:(?:\r\n?|\n)[ \t]*)*)` + `:`
//     + `(?:[ \t]*(?:(?:\r\n?|\n)[ \t]*)*)`. The delimiter is `:` ONLY: JSON's
//     only key/value delimiter is the colon, and the key requires MATCHING
//     double quotes (JSON keys are not single-quoted, and the pre-narrowing
//     `["']...["']` accepted mismatched quotes such as `"password'` and let a
//     quoted-diagnostic log line cross into the value span, redacting the
//     diagnostic message). A double-quoted key whose value (or colon) sits
//     on a later line is a machine-serialized
//     `{"password"\n:\n"hunter2secret"}`/`{"password":\n"hunter2secret"}`,
//     not a log line, and JSON permits arbitrary insignificant whitespace
//     (space, tab, CR, LF, CRLF, including blank lines) on BOTH sides of `:`.
//     `(?:\r\n?|\n)` matches a line break as CR, CRLF, or a standalone `\r`
//     (Go's encoding/json accepts a bare CR as whitespace), so the JSON form
//     consumes one newline OR many OR a blank line on either side of the
//     colon — recovering the multi-line-break and bare-CR JSON shapes the
//     pre-narrowing `\s*` covered — without re-opening the bare-key cross-line
//     guard, which this alternative cannot reach (it requires a quoted key
//     and `:`). The cross-line whitespace lives in the separator, so by the
//     time keyValueSecret's value alternation runs the value is contiguous
//     with the end of this group; the line structure (every introducing
//     newline) stays in the preserved prefix.
//
//  2. Loose form — `["']?[key]["']?[ \t]*[:=]` +
//     `(?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:[ \t]*(?:\r\n?|\n)[ \t]*)*[ \t]+)`. Keys
//     with optional surrounding quotes and either `:` or `=` (the TOML/INI/log
//     shape) keep the NARROW separator the cross-newline guard exists for.
//     Before the delimiter only `[ \t]*` (same-line horizontal whitespace) is
//     accepted — a bare or singly-quoted key cannot put the colon on a later
//     line, because that is the ambiguous bare-log-line shape `\s*`
//     over-redacted. (This also excludes a quoted key with `=` across lines:
//     JSON uses `:`, so a `"password"\n=\n"…"` log does not enter the JSON
//     form, and the loose form's `[ \t]*[:=]` cannot cross the newline before
//     the `=`; the unrelated quoted diagnostic on the next line survives.)
//     After the delimiter the first alternative `[ \t]*` keeps every real
//     same-line `<key> = <value>` / `<key>: <value>` (spaces, tabs, no
//     whitespace) and nothing across a line boundary. The second alternative
//     `[ \t]*(?:\r\n?|\n)(?:[ \t]*(?:\r\n?|\n)[ \t]*)*[ \t]+` narrows the
//     cross-line case to an INDENTED continuation: a value that begins on the
//     next line indented (`password:\n  hunter2secret`, the YAML/config
//     shape a log tail can paste) is a continuation of the key and is
//     redacted; a value at the left margin is an unrelated line and survives.
//     The line break `(?:\r\n?|\n)` matches CR, CRLF, or a standalone `\r`
//     — mirroring the JSON form — because YAML/config text can use any of
//     these as its line break (`password:\r  hunter2secret`, which the
//     pre-narrowing `\s*` covered but a straight `\r?\n` would drop, leaking
//     the credential). The zero-or-more run
//     `(?:[ \t]*(?:\r\n?|\n)[ \t]*)*` then consumes BLANK lines — including
//     WHITESPACE-ONLY blank lines (`password:\n  \n  hunter2secret`,
//     `password:\n\n  hunter2secret`, both covered by `\s*`) — by allowing
//     horizontal whitespace on BOTH sides of each intermediate line break
//     (the leading `[ \t]*` and the trailing `[ \t]*` per iteration); the
//     engine leaves the value line's own indent for the final `[ \t]+`
//     instead of consuming all of it. The REQUIRED `[ \t]+` BEFORE the
//     eventual value is the indent gate: it admits only a value whose first
//     line after the key/separator is indented, so a left-margin next line is
//     still an unrelated record (the bare-key cross-line guard pinned in
//     TestScrubCredentialKeyDoesNotCrossNewline). The leading `[ \t]*` is
//     the trailing-whitespace half of the separator: a config blob keeps
//     spaces/tabs after the `:`/`=` before the line break
//     (`password: \n  hunter2secret`), and without it neither alternative
//     matches — `[ \t]*` consumes the space but cannot cross the newline,
//     and a line break cannot follow the `:` through that space — so the
//     credential ships in the clear. The loose form has no column-0 cross-
//     line value separator at all — a value that begins at the left margin
//     on a later line is matched only by keyValueSecret's single-linebreak
//     value alternative (see keyValueSecret), which gates that case on a
//     quoted value and a SINGLE linebreak so a blank line still breaks the
//     association.
//
//  3. Flow form — `(?:\{[^{}]*?,|\{)[ \t]*["']?[key]["']?[ \t]*[:=]` +
//     `(?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:(?:\r\n?|\n)[ \t]*)*)`. Inside a YAML
//     flow collection (`{...}`) or after the `,` separating flow items,
//     indentation is not significant, so a value can legally begin at the
//     left margin (`{password:\nhunter2secret}`, `{a: 1, password:\ns3cr3t}`)
//     — a shape the bare-key loose form's `[ \t]*[:=]` cannot reach across
//     the newline, the single-linebreak value alternative requires a `"`, and
//     the JSON form requires a quoted key, so all three miss it; the
//     pre-narrowing `\s*` redacted it, so dropping it is a regression to the
//     leaking side. The flow form consumes the introducer — a `{` directly
//     before the key (`{password:…}`) OR a `,` reached THROUGH an opening
//     `{` (`\{[^{}]*?,`), so every comma-separated credential inside a
//     single flow collection is gated on an enclosing pair; WITHOUT a brace
//     a comma in log prose (`request failed, token:\n2026-01-01`) does NOT
//     enter this form, so the cross-line over-redaction the change is for
//     does not return. Both introducers live immediately before the key
//     (preserved as part of group 1 so the span engine redacts only the
//     value), and the separator tail then accepts arbitrary whitespace —
//     CR, CRLF, LF, blank lines, no indent, or indented — before the value,
//     so a bare, single-quoted, or double-quoted YAML flow value redacts
//     through keyValueSecret's same-line value alternatives (the line break
//     lives in the separator, so the line structure survives). The
//     `[^{}]*?,` alternative is lazy, so for a comma-separated credential
//     (`{a: 1, password:\nv}`) it consumes the SHORTEST prefix between the
//     opening `{` and the `,` that yields a credential marker, and same-line
//     credentials behind a `,` are still redacted by the loose form's
//     `["']?[key]["']?[ \t]*[:=]` (which does not gate on a `{`). The
//     bare-key cross-newline guard holds: a bare credential keyword ending
//     a log line has no `{` introducer AND no enclosing-flow `{` behind a
//     `,`, so the daemon-log SHA + commit-subject shape
//     (`checking token:\n4f2a…` …) cannot enter the flow form and survives
//     unchanged (pinned in TestScrubCredentialKeyDoesNotCrossNewline). Same-
//     line flow values (`{password: hunter2secret}`) are already redacted
//     by the loose form; the flow form additionally consumes the introducer
//     prefix and yields the same value span, deduplicated by the span engine.
//
//  4. Explicit-key YAML form — `\?[ \t]*[key][ \t]*(?:(?:\r\n?|\n)[ \t]*)*`
//     + `:` + `(?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:[ \t]*(?:\r\n?|\n)[ \t]*)*[ \t]+)`.
//     YAML's explicit mapping syntax places the colon on a LATER line and
//     marks the key with a leading `?` (`? password\n: hunter2secret`), a
//     shape the bare-key loose form's `[ \t]*[:=]` cannot reach (the colon
//     crosses a line), the JSON form requires a quoted key, and the flow
//     form's introducer requires a brace, so all miss it; the pre-narrowing
//     `\s*` covered it, so dropping it is a regression to the leaking side.
//     The explicit-key form gates the pre-colon cross-line widening on the
//     leading `?` (which means YAML explicit-key only): a bare log line
//     such as `request failed, token:\n2026-01-01` has no leading `?`, so
//     its bare-key colon stays on the same line as the key and the
//     cross-newline guard of the loose form holds (the unrelated next
//     record survives). The `(?:(?:\r\n?|\n)[ \t]*)*` between the key and
//     `:` consumes any line break (CR/CRLF/LF, including blank lines) the
//     same way the JSON form's separator does. AFTER the colon the
//     separator tail mirrors the loose form: same-line `[ \t]*` for
//     `? password: hunter2secret`, or the indented-continuation path for a
//     value placed on a later indented line (`? password\n: \n  hunter2secret`).
const credentialKeyPattern = `(?:(?:"[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?"(?:[ \t]*(?:(?:\r\n?|\n)[ \t]*)*):(?:[ \t]*(?:(?:\r\n?|\n)[ \t]*)*)|["']?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?["']?[ \t]*[:=](?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:[ \t]*(?:\r\n?|\n)[ \t]*)*[ \t]+)|(?:\{[^{}]*?,|\{)[ \t]*["']?[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?["']?[ \t]*[:=](?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:(?:\r\n?|\n)[ \t]*)*)|\?[ \t]*[a-z0-9_-]*(?:api[_-]?key|secret|token|password|passwd|pwd|auth|access[_-]?token|refresh[_-]?token|client[_-]?secret|bearer|credential|private[_-]?key)s?[ \t]*(?:(?:\r\n?|\n)[ \t]*)*:(?:[ \t]*|[ \t]*(?:\r\n?|\n)(?:[ \t]*(?:\r\n?|\n)[ \t]*)*[ \t]+)))`

// Beyond the same-line quoted/literal/bare value classes, keyValueSecret has
// one cross-line alternative: `(?:(?:\r\n?|\n)[ \t]*)("(?:\\.|[^"\\\r\n])*")` —
// a credential key (bare OR quoted) followed by a SINGLE line break (CR,
// CRLF, or LF, optionally then horizontal whitespace) and a column-0 QUOTED
// value. JSON and YAML/config serializers can place the value on the next
// line at the left margin (`{"password":\n"hunter2secret"}`,
// `token:\n"abcdefghijkl1234"` in a log tail — the bug-report bundle's
// collectLog and collectConfig paths), and the loose form's indented-only
// separator cannot reach a left-margin value, so this alternative redacts
// that shape. It is bounded on TWO sides to keep the bare-key cross-line
// guard the PR exists to enforce:
//
//   - SINGLE line break, NOT a run. The pre-narrowing `\s*` and an unbounded
//     `(?:\r?\n[ \t]*)*` both crossed a BLANK line (`token:\n\n"build failed"`)
//     and redacted an unrelated quoted message — the over-redaction this PR
//     was opened to stop. A single `(?:\r\n?|\n)` crosses one line boundary
//     and stops at a second, so a blank line still breaks the association.
//     The JSON FORM in credentialKeyPattern handles the genuine
//     multi-line-break JSON shape (`{"password":\n\n"…"}`, `{"password"\n\n:…`)
//     via its own separator, gated to a quoted key + `:`, so this value
//     alternative does not need the run and the bare-key blank-line guard
//     holds.
//   - Requires an opening `"`. A column-0 BARE token after a bare credential
//     key (`token:\n4f2a9c…`, the daemon-log SHA + commit-subject shape) has
//     no `"` and does not match — the same-line bare alternative
//     `[^\s"',}]{6,}` stops at the newline, so the unrelated log line
//     survives intact.
//
// The introducing `(?:\r\n?|\n)[ \t]*` is wrapped OUT of an inner capture so
// the line break stays in the separator/prefix and only the quoted value is
// the span: appendKeyValueSpans bounds the redaction on the inner group, so
// `token:\n"secret"` becomes `token:\n"[redacted-secret]"` and the log line
// structure (the key, the colon, the introducing newline) survives.
var keyValueSecret = regexp.MustCompile(
	`(?i)(` + credentialKeyPattern + `)(?:"(?:\\.|[^"\\\r\n])*"|'[^'\r\n]*'|[^\s"',}]{6,}|(?:(?:\r\n?|\n)[ \t]*)("(?:\\.|[^"\\\r\n])*"))`)

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
//
// Scrub is the log path's share of the shared normalization stage
// (internal/redactx): the shape matchers run on the line AND on every logical
// view the stage decodes out of it — a %q field's contents, ANSI-stripped hook
// output, a URI's percent-decoded components, a proven shell command's literal
// runs — so a credential no longer survives by sitting under an encoding this
// file does not know about (#4149). Each transform is gated on a byte that
// could begin its encoding, so a line carrying none of them pays only the
// flat matcher — see the benchmarks.
func Scrub(s string) string {
	return logStage.Scrub(s, redactx.ProvLogRecord)
}

// logStage is the credential-shape match policy plugged into the shared
// normalization stage. Every provenance the log family can decode into gets
// the same shape matchers; the stage owns which decodings are legal, this
// switch owns what is looked for in the result — the one place a new
// credential shape must be taught remains Redactions below.
var logStage = &redactx.Engine{
	Produce: func(text string, prov redactx.Provenance) []redactspan.Span {
		switch prov {
		case redactx.ProvLogRecord, redactx.ProvLogValue, redactx.ProvLogShell,
			redactx.ProvLogShellLiteral, redactx.ProvDiagnostic,
			redactx.ProvURIPathSensitive, redactx.ProvURIPathGeneric,
			redactx.ProvURIQueryPair, redactx.ProvURIComponent,
			redactx.ProvANSIPayload:
			return Redactions(text)
		default:
			return nil
		}
	},
	// A range the stage proved belongs to an encoding but could not decode is
	// an unknown logical value: redacted, not best-effort matched. This is
	// what makes the %q emitter's unparseable shell command or a malformed
	// opaque URI body lose its bytes rather than leak them.
	FailClosed: redactspan.Span{Replacement: RedactedMarker, Priority: 2},
	Fallback:   SecretMarker,
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
		// keyValueSecret's cross-line value alternative wraps the column-0
		// quoted value in an inner capture group
		// (`(?:(?:\r\n?|\n)[ \t]*)("…")`) so the single introducing line break
		// stays in the prefix and only the quoted value is the span. Use the
		// inner group (group 2) when that alternative participated; otherwise
		// (same-line quoted, single-quoted, or bare value, the indented
		// continuation, and the JSON-form separator's cross-line whitespace
		// case, where the value is contiguous with the end of group 1) the
		// loc[3]:loc[1] bounds are the complete, correct bounds.
		if len(loc) >= 6 && loc[4] >= 0 {
			start, end = loc[4], loc[5]
		}
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
