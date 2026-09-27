package bugreport

import (
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// saturatedPathContinuationEnd extends a saturated-scan path token that stopped
// at a delimiter through the rest of a path-bearing run, returning the index
// just past the run's last byte. On a Unix filesystem only NUL (and the path
// separator '/', which isPathTextDelimiter deliberately does not include)
// cannot appear inside a name, so every other isPathTextDelimiter rune is a
// filename-legal renderer terminator: a path could legitimately contain ';',
// ',', ':', '=', single quote, '(', ')', '[', ']', '{', '}', '<', '>', '&',
// '|', '`', space, tab, or any other Unicode whitespace, even though the renderer that
// emitted the surrounding prose used the same byte as a separator. The
// per-token scan that stops at the first such delimiter would blank only the
// prefix and strand the private suffix in a prose %q value such as
// recover_error (e.g. "/srv/Acme;Project/SecretRepo" stops at ';' and ships
// "Project/SecretRepo"). The run continues through path-legal bytes AND every
// filename-legal delimiter, ending at NUL (the one byte that cannot appear in
// a Unix filename) or the end of the decoded value. quoteStructural carries
// the scan's '"' handling: a structural '"' (a path-text delimiter or view
// boundary on one side, per saturatedQuoteIsStructural) ends the run, while a
// '"' flanked by path-legal bytes (an unquoted %s path such as
// /srv/Confidential"Client) is content and the run crosses it; on a decoded
// single-%q scalar (quoteStructural is false) the quotes are stripped and every
// '"' is content. Repeated spaces are filename-legal and do not end the run:
// the prior single-space-only extension shipped the suffix of a path such as
// "/srv/Acme  Project/SecretRepo" between the two spaces. Fail-closed: the
// saturated redactor can no longer tell an embedded path from prose, so
// erring toward the marker preserves the privacy contract at the cost of
// layout only that degenerate case ever had (#4938 review).
func saturatedPathContinuationEnd(s string, delimiterEnd int, quoteStructural bool) int {
	end := delimiterEnd
	for end < len(s) {
		c, size := utf8.DecodeRuneInString(s[end:])
		if c == '\x00' {
			break
		}
		if quoteStructural && c == '"' && saturatedQuoteIsStructural(s, end) {
			// A structural '"' ends the path UNLESS it opens a filename quote
			// pair whose matching close is followed by a delimiter that is
			// interior to the path (a filename-legal isPathTextDelimiter rune
			// the path continues past, such as the space inside
			// `/srv/Acme "Secret" Client`). saturatedQuoteIsStructural flags
			// that opener as structural because the close is followed by a
			// delimiter, but the trailing delimiter is path content, not a
			// value boundary. Jump the opener...close pair whole and keep
			// scanning the unquoted tail instead of stranding the suffix after
			// the opener. A real %q closing quote reached directly still stops
			// the scan via the clause above: it is not an opener, so the jump
			// declines (#4938 review).
			if jump := saturatedPathQuotePairJump(s, end); jump > end {
				end = jump
				continue
			}
			break
		}
		end += size
	}
	return end
}

// saturatedPathQuotePairJump reports the offset just past the matched Go %q
// close of the '"' at i when that close is followed by a delimiter interior to
// a path the saturated path scan is blanking, so the scan can jump the
// opener...close pair whole and keep scanning the unquoted tail. A legal Unix
// filename can carry a double quote and the respawn logger emits the workDir
// with %s, so a past-the-cap worktree such as `/srv/Acme "Secret" Client`
// reaches the whole daemon-log record verbatim (quoteStructural). The space
// before the opener and after the pair's close is interior to the name, but
// saturatedQuoteIsStructural flags the opener as structural because its
// matching close is followed by a delimiter (the space); without this jump the
// saturated scan stops at the opener and strands `"Secret" Client` in the
// daemon tail. Only an opener (its after-byte is path content, not a
// delimiter) whose close is followed by a delimiter that the path continues
// past (the byte after that delimiter is path content, not another delimiter,
// NUL, or end of view) jumps; a real %q closing quote is not an opener and a
// pair close at a genuine boundary does not (#4938 review).
func saturatedPathQuotePairJump(s string, i int) int {
	if i < 0 || i+1 >= len(s) || s[i] != '"' {
		return i
	}
	after, _ := utf8.DecodeRuneInString(s[i+1:])
	if after == '\x00' || isPathTextDelimiter(after) {
		return i // a closing quote (after is a delimiter) is not an opening pair
	}
	end := redactx.GoQuotedEnd(s, i) // offset just past the matched close '"'
	if end < 0 || end >= len(s) {
		return i
	}
	afterClose, asize := utf8.DecodeRuneInString(s[end:])
	if afterClose == '\x00' || !isPathTextDelimiter(afterClose) {
		return i // path-data pair the classifier already crossed, or NUL after close
	}
	next := end + asize
	if next >= len(s) {
		return i // trailing delimiter at end of view is a genuine boundary
	}
	nextRune, _ := utf8.DecodeRuneInString(s[next:])
	if nextRune == '\x00' || isPathTextDelimiter(nextRune) {
		return i // delimiter-delim or delimiter-NUL is a genuine path boundary
	}
	return end // trailing delimiter is interior: jump the pair
}
