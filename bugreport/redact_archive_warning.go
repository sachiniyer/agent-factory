package bugreport

import (
	"regexp"
	"strconv"
	"strings"
)

// The incomplete-archive warning, in both places it reaches a bundle: the
// bounded ArchiveWarning field and the daemon log line rendered from the same
// report. Both go through the matchers below, so the two copies of one string
// cannot end up with two policies (#3541, #3554, #3588).

// The incomplete-archive warning is the one daemon log line that prints the
// names of files a USER chose. session/git renders it in a single place
// (ArchiveReport.warningSuffix), in a single shape:
//
//	<operation> completed with an incomplete archive: af skipped <N> unreadable
//	files; complete original tree(s) were retained at "<root>"[, "<root>"][, and
//	<N> more in archive_report]; skipped paths[ (showing first <X> of <Y>)]:
//	"<path>" (<reason>)[, "<path>" (<reason>)]
//
// The matchers below key on THAT SHAPE and on nothing else — no live
// ArchiveReport, no note* call, no per-run state — because the log outlives the
// state such a key would come from (#3553): an instances.json that fails
// the typed decode never reaches the typed walk that could record the paths, a
// session killed after the warning was written leaves neither report nor row
// behind, and a bundled tail can hold lines an older binary wrote. Reading the
// renderer instead is also what makes ONE pass over the (up to 2 MiB) tail
// enough, however many unreadable files a generated tree produced.
//
// The renderer being the only source of this shape is the cost of that: a change
// to the format silently stops the scrub. It is paid down in the tests, which
// build every input from the real ArchiveReport.Warning rather than from a
// hand-written fixture, so format and matcher cannot drift apart without a red
// test.
const (
	// archiveWarningQuotedPattern matches one %q-rendered value. strconv.Quote
	// escapes every interior `"` as `\"` and every `\` as `\\`, so this is an
	// exact tokenizer for the renderer's output, not an approximation of one.
	archiveWarningQuotedPattern = `"(?:[^"\\]|\\.)*"`
	archiveWarningRetainedItem  = `(?:` + archiveWarningQuotedPattern + `|and \d+ more in archive_report)`
	// The reason field admits no `"` — session/git takes quotes, parens and
	// control characters out of an unknown reason before rendering it, precisely
	// so each entry stays delimitable. Requiring here what the emitter
	// guarantees is what makes a line from a binary older than that change fail
	// the grammar and take the whole-remainder fallback, instead of parsing into
	// a walk that an ODD number of quotes puts out of step — which would pair a
	// reason's quote with the NEXT entry's opening one and ship that entry's name.
	archiveWarningSkippedItem = archiveWarningQuotedPattern + ` \([^()"\r\n]*\)`
)

var (
	// archiveWarningRetainedAt finds one warning and captures the rest of its
	// line. It anchors on the renderer's most distinctive literal, which sits
	// immediately before the first path; %q never emits a raw newline, so the
	// whole clause is always on that one line, and collectLog drops the partial
	// first line of a truncated tail, so a bundled clause is never half a line.
	archiveWarningRetainedAt = regexp.MustCompile(`complete original tree\(s\) were retained at ([^\r\n]*)`)
	// archiveWarningTail is the renderer's grammar for that remainder, in four
	// groups: the retained-root list, the label between them, the skipped-path
	// list (empty when a tree was retained with nothing recorded under it), and
	// whatever a caller appended to the same line.
	//
	// That last group is why the invariant holds: it admits no `"` at all, so
	// EVERY quoted token in a parsed remainder sits in group 1 or group 3 and is
	// rewritten. A wrapper that adds quote-free text — `failedArchiveWithHook`
	// appends "(its on-archive hook also failed: …)" to a joined error whose last
	// line is this warning — keeps its diagnostics; one that adds a quoted token
	// fails the grammar and takes the fail-safe path in redactArchiveWarningTail.
	archiveWarningTail = regexp.MustCompile(
		`\A(` + archiveWarningRetainedItem + `(?:, ` + archiveWarningRetainedItem + `)*)` +
			`(; skipped paths(?: \(showing first \d+ of \d+\))?: )` +
			`((?:` + archiveWarningSkippedItem + `(?:, ` + archiveWarningSkippedItem + `)*)?)` +
			`([^"\r\n]*)\z`)
	archiveWarningQuoted = regexp.MustCompile(archiveWarningQuotedPattern)
)

// scrubArchiveWarningPaths takes the user-chosen file names out of every
// incomplete-archive warning in the text it is given, and collapses the retained
// root printed beside them.
//
// WHICH warning it finds still comes from the renderer and nothing else — see
// the matchers above for why keying on the format rather than on live session
// state is the point. What the run supplies is only the ROOT policy: naming a
// directory takes the roots this redactor registered, exactly as the path fields
// do (#3588), so the root a warning prints and the ArchiveReport field it was
// rendered from cannot end up with two different treatments.
func (r *redactor) scrubArchiveWarningPaths(s string) string {
	clauses := archiveWarningRetainedAt.FindAllStringSubmatchIndex(s, -1)
	if clauses == nil {
		return s
	}
	var out strings.Builder
	copied := 0
	for _, clause := range clauses {
		// clause[2]:clause[3] is the captured remainder of the line; everything
		// before it (the anchor prose included) is copied through untouched.
		out.WriteString(s[copied:clause[2]])
		out.WriteString(r.redactArchiveWarningTail(s[clause[2]:clause[3]]))
		copied = clause[3]
	}
	out.WriteString(s[copied:])
	return out.String()
}

// redactArchiveWarningTail rewrites what one warning prints after "retained
// at ": the retained roots, then the skipped file names with their reasons.
//
// The two lists are treated differently because the policy for the two VALUES
// differs, and this keeps the log agreeing with the JSON section instead of
// inventing a second policy for the same fields:
//
//   - A skipped path is a RELATIVE name a user chose for a file af could not
//     read, so it goes — exactly as redactInstanceData blanks
//     archive_report.retained_trees[].skipped[].path.
//   - A retained root is a SYSTEM path, which that same function COLLAPSES to
//     the token of the root it hangs off — or blanks, when it hangs off none
//     this run knows. Two steps make that possible here. The %q escaping is the
//     first: a root that is not valid UTF-8 arrives as `\xNN` escapes, which no
//     path comparison can see through, so the token is rewritten to the DISPLAY
//     spelling the JSON copy carries (for an ordinary valid-UTF-8 root that
//     rewrite is the identity). The second is collapsePathField itself, applied
//     to the result — which is what stops the log from keeping a directory name
//     the structured section beside it just dropped.
//
// A remainder that does not parse as the renderer's grammar is dropped WHOLE.
// The format moved, or something appended a quoted token of its own; either way
// the quoted runs can no longer be told apart, and a bundle that loses a skip
// reason is a nuisance where one that ships a user's file names is the bug being
// fixed. Between the grammar and that fallback, no quoted token in a matched
// clause can reach the bundle unrewritten.
func (r *redactor) redactArchiveWarningTail(tail string) string {
	parsed := archiveWarningTail.FindStringSubmatchIndex(tail)
	if parsed == nil {
		return redactedMarker
	}
	var out strings.Builder
	out.WriteString(archiveWarningQuoted.ReplaceAllStringFunc(
		tail[parsed[2]:parsed[3]], r.archiveWarningRetainedRoot))
	// The label, kept verbatim: it carries the "(showing first X of Y)" count
	// triage reads. Like the trailing group, it holds no quoted token.
	out.WriteString(tail[parsed[4]:parsed[5]])
	out.WriteString(archiveWarningQuoted.ReplaceAllStringFunc(
		tail[parsed[6]:parsed[7]],
		func(string) string { return strconv.Quote(redactedMarker) }))
	out.WriteString(tail[parsed[8]:parsed[9]])
	return out.String()
}

// archiveWarningRetainedRoot rewrites one %q-rendered retained root: first to
// the display spelling of the same path — the form encoding/json can carry, and
// the form a path comparison can match — and then through the same root policy
// every path field takes. A token that will not unquote did not come from this
// renderer, so it is redacted rather than guessed at.
func (r *redactor) archiveWarningRetainedRoot(token string) string {
	raw, err := strconv.Unquote(token)
	if err != nil {
		return strconv.Quote(redactedMarker)
	}
	return strconv.Quote(r.collapsePathField(strings.ToValidUTF8(raw, "\uFFFD")))
}

// redactArchiveWarning takes the user-chosen file names out of the bounded
// incomplete-archive warning FIELD, using the same renderer-keyed function the
// log pass uses.
//
// Text carrying no retained-at clause is dropped WHOLE, and that is not a
// defensive maybe: ArchiveReport.warningSuffix is one format string, and every
// warning it produces contains that clause. Text without it did not come from
// that renderer — an older binary, a wrapper, a hand-edited record — so its
// quoted runs cannot be told apart, which is the same reason
// redactArchiveWarningTail drops a remainder that fails its grammar.
func (r *redactor) redactArchiveWarning(warning string) string {
	if warning == "" {
		return ""
	}
	if !archiveWarningRetainedAt.MatchString(warning) {
		return redactedMarker
	}
	return r.scrubArchiveWarningPaths(warning)
}
