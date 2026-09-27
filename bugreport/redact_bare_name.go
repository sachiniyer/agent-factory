package bugreport

import (
	"path/filepath"
	"unicode/utf8"
)

// appendSaturatedBareNameSpans is the fail-closed scan the bare-name cap
// switches appendBareNameLogOnlyPathBlankSpans to on quoteStructural views
// (a whole daemon-log record, a diagnostic, a shell command) once
// noteLogOnlyPathRedaction reaches maxLogOnlyPathBlanks on the bare-name set.
// The capped per-needle scan cannot reach a past-the-cap bare name (dropped),
// and the slash-bearing saturated scan cannot anchor on a '/' a bare name does
// not have, so a past-the-cap bare name logged UNQUOTED via %s
// (session/backend_local_respawn.go's "at %s" workDir) — which has no decoded
// %q view to fall through to the scalar blank — would ship verbatim once the
// record also carries any '/' (the "branch %s" with feature/foo), since the
// earlier no-'/' whole-record blank suppressed at the '/' gate. This single
// O(text) pass walks the view and blanks every unquoted bare-name-shaped token
// at a bareBoundary instead, so the dropped name does not survive while the
// scan stays bounded by the text size.
//
// "Bare-name-shaped" is a maximal run of path-text-legal bytes that carries no
// '/' and sits outside a '"' quoted region: the per-scalar pass already
// handles every decoded %q scalar (it blanks a no-'/' scalar whole and leaves a
// '/'-bearing scalar to the slash-bearing scan), so a '"' on this
// quoteStructural view is the structural terminator of one such scalar and the
// scan does not cross it — it blanks only the UNQUOTED bytes the scalar pass
// cannot reach, the way appendSaturatedLogOnlyPathBlankSpans terminates its
// walkback at '"' on the same views. The first segment of an unquoted
// '/'-bearing token (the "feature" of "feature/foo") blanks: it starts at a
// text boundary and the bare name the cap dropped is exactly that shape.
// Deeper segments are preceded by '/', which is not a text delimiter, so
// pathStartsAt rejects them and they survive the slash-bearing scan — a bare
// name never carries a '/' by definition, so the one segment that can be a
// dropped name is the one this scan reaches (#4938 review).
//
// The over-blank — emitter labels, scheme names, and the first segments of
// unquoted '/'-bearing tokens blanking in a '/'-bearing record, in the
// degenerate archive that saturates the bare-name set — is the privacy side
// of the same fail-closed trade the slash-bearing saturated scan already makes
// for every '/'-bearing token (#4938 review).
func (r *redactor) appendSaturatedBareNameSpans(
	spans []redactionSpan,
	s string,
	bareBoundary pathBoundary,
) []redactionSpan {
	inQuote := false
	i := 0
	for i < len(s) {
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == '"' {
			// A '"' is the structural terminator of a %q value on a
			// quoteStructural view; the per-scalar pass handles the decoded
			// scalar, so toggle out of the quoted region and never blank
			// inside it. The same terminator appendSaturatedLogOnlyPathBlankSpans
			// stops its walkback at (#4938 review).
			inQuote = !inQuote
			i += size
			continue
		}
		if inQuote || isPathTextDelimiter(c) || c == filepath.Separator {
			// Inside a quoted scalar, or at a path-text delimiter (which a bare
			// name cannot contain), or at a path separator (which separates a
			// bare name from a deeper segment this scan must not own): none of
			// these can begin a bare-name token, so skip. A '/' skipped here
			// leaves the next segment preceded by '/', which pathStartsAt
			// rejects, so deeper segments survive (#4938 review).
			i += size
			continue
		}
		start := i
		for i < len(s) {
			c, size := utf8.DecodeRuneInString(s[i:])
			if c == '"' || isPathTextDelimiter(c) || c == filepath.Separator {
				break
			}
			i += size
		}
		end := i
		if end > start && bareBoundary(s, start, end) {
			spans = append(spans, redactionSpan{
				start: start, end: end, replacement: redactedMarker, priority: spanQuotedValue,
			})
		}
	}
	return spans
}
