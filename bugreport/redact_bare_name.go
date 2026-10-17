package bugreport

import (
	"path/filepath"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// saturatedQuoteIsStructural reports whether the '"' at i in s is a structural
// %q delimiter on a quoteStructural view, as opposed to a literal '"' that is
// filename content in an UNQUOTED %s emitter value (e.g. a workDir
// /srv/Confidential"Client that session/backend_local_respawn.go logs via %s).
// A path-data '"' flanked by filename-legal bytes on both sides is content. A
// structural '"' is a valid Go %q delimiter: a closing quote followed by a
// path-text delimiter or a view boundary, or an opening quote (preceded by a
// path-text delimiter) whose matching Go %q close is itself followed by a
// delimiter or view boundary. The saturated scans cross a '"' only when it is
// content (a filename quote pair or a quote flanked by path-legal bytes) and
// stop at a real value boundary, so an unquoted path blanks whole instead of
// stranding its suffix after the quote, while a real %q closing quote still
// protects the triage after it (#4938 review).
//
// The opening-quote validation is the #4938 review fix for a space-prefixed
// filename quote. A legal Unix filename can carry a double quote, and the
// respawn logger emits the workDir with %s, so a past-the-cap worktree such as
// `/srv/Acme "Secret"Client` reaches the whole daemon-log record verbatim. The
// space before the '"' used to make this function classify it as a structural
// %q opener (a path-text delimiter on one side), so saturatedPathContinuationEnd
// stopped there and blanked only "/srv/Acme ", shipping `Secret"Client` in the
// daemon tail — the quote transform could then also redact "Secret" as an
// unrelated scalar, but the surrounding filename content still named the
// private directory. A '"' that a path-text delimiter precedes is now the
// opening of a %q field only when its matching Go %q close (GoQuotedEnd) is
// itself followed by a delimiter or view boundary, so a filename quote pair
// such as "Secret" whose close is followed by more filename content ("Client")
// is path data and the saturated scan crosses the pair whole. GoQuotedEnd
// terminates at the next newline, so on the line-oriented daemon-log view the
// look-ahead is bounded by the line (#4938 review).
func saturatedQuoteIsStructural(s string, i int) bool {
	if i < 0 || i >= len(s) || s[i] != '"' {
		return false
	}
	if i == 0 || i+1 >= len(s) {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[i+1:])
	// A '"' followed by a path-text delimiter is the closing quote of a %q
	// field (e.g. key="value" ) — structural.
	if isPathTextDelimiter(after) {
		return true
	}
	// A '"' flanked by filename-legal bytes on both sides is filename content
	// in an unquoted %s value (e.g. /srv/Confidential"Client) — not structural.
	if !isPathTextDelimiter(before) {
		return false
	}
	// A '"' preceded by a path-text delimiter but followed by filename-legal
	// bytes is the opening of a %q field only if the matching Go %q close is
	// itself followed by a delimiter or view boundary. A filename quote pair
	// such as the "Secret" in `/srv/Acme "Secret"Client` has its close
	// followed by more filename content ("Client"), so the pair is path data
	// and the saturated scan must cross it instead of stopping at the
	// space-prefixed opener and stranding the suffix (#4938 review).
	end := redactx.GoQuotedEnd(s, i)
	if end < 0 {
		return false
	}
	if end >= len(s) {
		return true
	}
	afterClose, _ := utf8.DecodeRuneInString(s[end:])
	return isPathTextDelimiter(afterClose)
}

// appendSaturatedBareNameSpans is the fail-closed scan the bare-name cap
// switches appendBareNameLogOnlyPathBlankSpans to once
// noteLogOnlyPathRedaction reaches maxLogOnlyPathBlanks on the bare-name set,
// and that the worktree-title cap also reaches when a fallback sibling shape
// would otherwise escape (appendLogOnlyPathBlankSpansWithBoundary). The
// capped per-needle scan cannot reach a past-the-cap bare name (dropped), and
// the slash-bearing saturated scan cannot anchor on a '/' a bare name does
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
// '/'-bearing scalar to the slash-bearing scan), so a '"' on a quoteStructural
// view is the structural terminator of one such scalar and the scan does not
// cross it — it blanks only the UNQUOTED bytes the scalar pass cannot reach,
// the way appendSaturatedLogOnlyPathBlankSpans terminates its walkback at '"'
// on the same views. The first segment of an unquoted '/'-bearing token (the
// "feature" of "feature/foo") blanks: it starts at a text boundary and the
// bare name the cap dropped is exactly that shape. Deeper segments are
// preceded by '/', which is not a text delimiter, so pathStartsAt rejects
// them and they survive the slash-bearing scan — a bare name never carries a
// '/' by definition, so the one segment that can be a dropped name is the one
// this scan reaches (#4938 review).
//
// quoteStructural carries the same '"' handling the saturated path scan
// threads from produceSpans. On a whole-record view (quoteStructural) a '"'
// toggles the quoted region only when it is a structural %q delimiter
// (saturatedQuoteIsStructural): a path-data '"' flanked by filename-legal
// bytes — an unquoted %s workDir such as Confidential"Client — is content, not
// a delimiter, so it is included in the surrounding bare-name token and the
// whole private name blanks instead of stranding "Client after the quote. On
// a decoded single-%q scalar (ProvLogValue, quoteStructural=false) the
// surrounding quotes are already stripped and every '"' is filename content,
// so it never toggles; this is the view the '/'-bearing recover_error scalar
// reaches when the bare-name set is saturated, so a past-the-cap bare name
// sitting in such a scalar (a wrapped stat error beside a branch ref) blanks
// too instead of leaking through the '/' gate (#4938 review).
//
// The quoted-region toggle tracks Go %q escaping rather than flipping on every
// '"' byte: a session title such as `fix"bug` is emitted as the %q value
// `"fix\"bug"`, so the interior `\"` is an escaped quote that is data, not the
// structural terminator. A naive toggle treats that escaped quote as the
// closer and the real closing quote as the next opener, leaving inQuote true
// for the unquoted bytes that follow (for example `at ConfidentialClient4097`
// after the title), so the past-the-cap bare name logged beside it survives
// verbatim instead of being blanked. The escape flag mirrors GoQuotedEnd: a
// backslash inside the quoted region consumes the next byte as escaped, so an
// escaped `\"` does not toggle while a real closing `"` still does (#4938
// review).
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
	quoteStructural bool,
) []redactionSpan {
	inQuote := false
	// escaped tracks Go %q backslash escaping, but only inside a quoted
	// region: outside a quote a '\' is a filename-legal byte that may start
	// or continue a bare-name token, so it is not an escape there.
	escaped := false
	i := 0
	for i < len(s) {
		c, size := utf8.DecodeRuneInString(s[i:])
		if inQuote {
			if escaped {
				escaped = false
				i += size
				continue
			}
			if c == '\\' {
				escaped = true
				i += size
				continue
			}
			if c == '"' {
				// The real structural terminator of the %q value: the
				// per-scalar pass handles the decoded scalar, so leave the
				// quoted region. An escaped `\"` was consumed above and
				// never reached this toggle (#4938 review).
				inQuote = false
			}
			i += size
			continue
		}
		if quoteStructural && c == '"' && saturatedQuoteIsStructural(s, i) {
			// A structural '"' on a quoteStructural view opens a %q value;
			// the per-scalar pass handles the decoded scalar, so enter the
			// quoted region and never blank inside it. The same terminator
			// appendSaturatedLogOnlyPathBlankSpans stops its walkback at
			// (#4938 review).
			inQuote = true
			i += size
			continue
		}
		if c == '"' {
			// A '"' that is not structural is filename content in an unquoted
			// %s value (a workDir such as Confidential"Client on the whole
			// record) or any '"' on a decoded %q scalar (ProvLogValue, where
			// the surrounding quotes are already stripped): it begins or
			// continues a bare-name token, so fall through to the token scan
			// rather than treating it as a delimiter and stranding the suffix
			// after the quote (#4938 review).
		} else if isPathTextDelimiter(c) || c == filepath.Separator {
			// At a path-text delimiter (which a bare name cannot contain), or
			// at a path separator (which separates a bare name from a deeper
			// segment this scan must not own): none of these can begin a
			// bare-name token, so skip. A '/' skipped here leaves the next
			// segment preceded by '/', which pathStartsAt rejects, so deeper
			// segments survive (#4938 review).
			i += size
			continue
		}
		start := i
		for i < len(s) {
			c, size := utf8.DecodeRuneInString(s[i:])
			if c == '"' {
				if quoteStructural && saturatedQuoteIsStructural(s, i) {
					// The structural terminator of a %q value ends the
					// token; the outer loop enters the quoted region next.
					break
				}
				// Path-data '"': include it in the bare-name token and keep
				// scanning (#4938 review).
				i += size
				continue
			}
			if isPathTextDelimiter(c) || c == filepath.Separator {
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
