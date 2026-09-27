package bugreport

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/internal/credscrub"
	"github.com/sachiniyer/agent-factory/internal/redactspan"
	"github.com/sachiniyer/agent-factory/internal/redactx"
)

// redactionSpan is one replacement located in the unmodified input. Credentials,
// names, paths, and transport-decoded values are independent views of the same
// text; recording all matches first keeps one producer from destroying another's
// evidence.
type redactionSpan struct {
	start       int
	end         int
	replacement string
	priority    int
}

const (
	spanKnownLabel = iota
	spanQuotedTitle
	spanLegacyTitle
	spanTmuxName
	spanWorktreeTitle
	spanKnownRoot
	spanCredential
	spanUsername
	spanQuotedValue
)

// produceSpans is this redactor's match policy on one logical view — what
// counts as secret in text carrying a given provenance. The shared engine
// (internal/redactx) decides which decodings a provenance admits; this switch
// decides which matchers see the decoded result. It mirrors the per-context
// matcher sets the old dispatcher hardcoded: the log family gets the
// sensitive (known-value) matchers plus legacy emitter titles, shell commands
// get their path boundaries beside the family matchers, and decoded URI
// components get their dedicated boundary rules.
func (r *redactor) produceSpans(text string, prov redactx.Provenance) []redactionSpan {
	switch prov {
	case redactx.ProvLogRecord, redactx.ProvLogValue:
		return appendLegacyTaskTitleSpans(r.sensitiveBaseTextSpans(text), text)
	case redactx.ProvLogShell:
		spans := appendLegacyTaskTitleSpans(r.sensitiveBaseTextSpans(text), text)
		return append(spans, r.shellBoundarySpans(text)...)
	case redactx.ProvLogShellRaw:
		return r.shellBoundarySpans(text)
	case redactx.ProvDiagnostic, redactx.ProvLogShellLiteral, redactx.ProvANSIPayload,
		redactx.ProvURIQueryPair, redactx.ProvURIComponent:
		return r.sensitiveBaseTextSpans(text)
	case redactx.ProvGeneric, redactx.ProvConfigScalar, redactx.ProvConfigShellLiteral:
		return r.genericBaseTextSpans(text)
	case redactx.ProvConfigShell:
		return append(r.genericBaseTextSpans(text), r.shellBoundarySpans(text)...)
	case redactx.ProvURIPathSensitive:
		return r.sensitiveURIPathTextSpans(text)
	case redactx.ProvURIPathGeneric:
		return r.genericURIPathTextSpans(text)
	default:
		return nil
	}
}

// shellBoundarySpans matches the registered path roots and worktree titles a
// proven shell command can name. Boundary recognition needs the command's own
// parse context — a registered path may end where an expansion begins — so the
// producer re-derives it from the command text. When the shell literal
// transform already failed the parse, this returns nil: the transform's
// fail-closed range covers the whole command, and degraded boundary checks on
// it cannot leak anything it did not already take.
func (r *redactor) shellBoundarySpans(command string) []redactionSpan {
	context, ok := redactx.ParseShell(command)
	if !ok {
		return nil
	}
	endsAt := func(s string, start, end int) bool {
		return pathEndsAt(s, start, end) || shellExpansionEndsAt(context, s, start, end)
	}
	worktreeBoundary := func(s string, start, end int) bool {
		return derivedWorktreePathBoundaryWithEnd(s, start, end, endsAt)
	}
	rootBoundary := func(s string, start, end int) bool {
		return knownRootTextBoundaryWithEnd(s, start, end, endsAt)
	}
	spans := r.appendWorktreePathTitleSpansWithBoundary(nil, command, worktreeBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, command, worktreeBoundary)
	return r.appendKnownRootSpansWithBoundary(spans, command, rootBoundary)
}

// shellExpansionEndsAt reports whether a candidate path may end at end because
// a shell expansion materializes there — or, after word-owned line
// continuations, a path delimiter does.
func shellExpansionEndsAt(c redactx.ShellContext, s string, start, end int) bool {
	if c.DirectExpansionStartsAt(start, end) {
		return true
	}
	next, ok := c.AfterLineContinuations(start, end)
	if !ok {
		return false
	}
	return pathEndsAt(s, start, next) || c.DirectExpansionStartsAt(start, next)
}

// toSharedSpans adapts the package-local span type to the shared stage's
// span. The local type survives because every matcher below was written
// against it; the boundary converts once per view.
func toSharedSpans(spans []redactionSpan) []redactspan.Span {
	out := make([]redactspan.Span, 0, len(spans))
	for _, span := range spans {
		out = append(out, redactspan.Span{
			Start: span.start, End: span.end,
			Replacement: span.replacement, Priority: span.priority,
		})
	}
	return out
}

func (r *redactor) sensitiveBaseTextSpans(s string) []redactionSpan {
	spans := r.knownBaseTextSpans(s)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

func (r *redactor) genericBaseTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendAccountLabelSpans(spans, s)
	spans = r.appendWorktreePathTitleSpans(spans, s)
	spans = r.appendWorktreeSubdirectoryTitleSpans(spans, s)
	spans = r.appendKnownRootSpans(spans, s)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

func (r *redactor) knownBaseTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendKnownLabelSpans(spans, s)
	spans = r.appendTmuxNameSpans(spans, s)
	spans = r.appendWorktreePathTitleSpans(spans, s)
	spans = r.appendWorktreeSubdirectoryTitleSpans(spans, s)
	spans = r.appendKnownRootSpans(spans, s)
	spans = r.appendLogOnlyPathBlankSpans(spans, s)
	return spans
}

func (r *redactor) sensitiveURIPathTextSpans(s string) []redactionSpan {
	spans := r.knownURIPathTextSpans(s)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

func (r *redactor) genericURIPathTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendAccountLabelSpans(spans, s)
	spans = r.appendWorktreePathTitleSpansWithBoundary(spans, s, uriWorktreePathBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, s, uriWorktreePathBoundary)
	spans = r.appendKnownRootSpansWithBoundary(spans, s, uriKnownRootBoundary)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

func (r *redactor) knownURIPathTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendKnownLabelSpans(spans, s)
	spans = r.appendTmuxNameSpans(spans, s)
	spans = r.appendWorktreePathTitleSpansWithBoundary(spans, s, uriWorktreePathBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, s, uriWorktreePathBoundary)
	spans = r.appendKnownRootSpansWithBoundary(spans, s, uriKnownRootBoundary)
	spans = r.appendLogOnlyPathBlankSpansWithBoundary(spans, s, uriKnownRootBoundary, uriWorktreePathBoundary)
	return spans
}

func (r *redactor) appendKnownLabelSpans(spans []redactionSpan, s string) []redactionSpan {
	spans = r.appendAccountLabelSpans(spans, s)
	return r.appendTitleSpans(spans, s)
}

// appendTitleSpans treats titles as user-authored free text. Exact %q forms
// cover every legal byte sequence; bare forms require a whole word-bearing
// token so punctuation-only titles do not erase ordinary syntax globally.
func (r *redactor) appendTitleSpans(spans []redactionSpan, s string) []redactionSpan {
	for title := range r.titles {
		quoted := strconv.Quote(title)
		spans = appendExactSpans(spans, s, quoted, strconv.Quote(redactedMarker), spanQuotedTitle)
		spans = appendTokenSpans(spans, s, title, redactedMarker, isWordRune, spanKnownLabel)
	}
	return spans
}

// appendAccountLabelSpans removes a registered, user-chosen account name only
// as a whole label. The account alphabet keeps "work" useful inside the
// unrelated branch value "work-stuff" while still removing `--account work`.
func (r *redactor) appendAccountLabelSpans(spans []redactionSpan, s string) []redactionSpan {
	for label := range r.accounts {
		spans = appendTokenSpans(spans, s, label, redactedMarker, isAccountNameRune, spanKnownLabel)
	}
	return spans
}

// appendUsernameSpans uses a whole word-bearing token instead of regexp \b, so
// an OS username ending in punctuation (for example "test-") is still removed
// from a branch path without erasing the same bytes inside a longer word.
func (r *redactor) appendUsernameSpans(spans []redactionSpan, s string) []redactionSpan {
	for _, name := range r.users {
		spans = appendTokenSpans(spans, s, name, userMarker, isWordRune, spanUsername)
	}
	return spans
}

func appendCredentialSpans(spans []redactionSpan, s string) []redactionSpan {
	for _, credential := range credscrub.Redactions(s) {
		spans = append(spans, redactionSpan{
			start: credential.Start, end: credential.End,
			replacement: credential.Replacement, priority: spanCredential,
		})
	}
	return spans
}

func (r *redactor) appendTmuxNameSpans(spans []redactionSpan, s string) []redactionSpan {
	for _, loc := range afTmuxSessionName.FindAllStringIndex(s, -1) {
		match := s[loc[0]:loc[1]]
		spans = append(spans, redactionSpan{
			start: loc[0], end: loc[1], replacement: redactAFTmuxTitle(match), priority: spanTmuxName,
		})
	}
	for name := range r.tmuxNames {
		if !afTmuxSessionName.MatchString(name) {
			spans = appendExactSpans(spans, s, name, tmuxPrefixMarker, spanTmuxName)
		}
	}
	return spans
}

func appendLegacyTaskTitleSpans(spans []redactionSpan, s string) []redactionSpan {
	for _, loc := range taskStartedInstanceTitle.FindAllStringSubmatchIndex(s, -1) {
		spans = append(spans, redactionSpan{
			start: loc[3], end: loc[1], replacement: redactedMarker, priority: spanLegacyTitle,
		})
	}
	for _, loc := range taskParkedInstanceTitle.FindAllStringSubmatchIndex(s, -1) {
		spans = append(spans, redactionSpan{
			start: loc[4], end: loc[5], replacement: redactedMarker, priority: spanLegacyTitle,
		})
	}
	return spans
}

func (r *redactor) appendWorktreePathTitleSpans(spans []redactionSpan, s string) []redactionSpan {
	return r.appendWorktreePathTitleSpansWithBoundary(spans, s, derivedWorktreePathBoundary)
}

func (r *redactor) appendWorktreePathTitleSpansWithBoundary(
	spans []redactionSpan,
	s string,
	boundary pathBoundary,
) []redactionSpan {
	for title := range r.worktreePathTitles {
		needle := title.repoPath + "-" + title.segment
		scan := 0
		for scan <= len(s)-len(needle) {
			rel := strings.Index(s[scan:], needle)
			if rel < 0 {
				break
			}
			start := scan + rel
			end := start + len(needle)
			if boundary(s, start, end) {
				titleStart := start + len(title.repoPath) + 1
				spans = append(spans, redactionSpan{
					start: titleStart, end: end, replacement: redactedMarker, priority: spanWorktreeTitle,
				})
				// A registered repo followed by this proven title segment is the
				// intentional sibling shape, not an unrelated prefix collision. Add
				// its root span even though the ordinary root boundary rejects '-'.
				if token, ok := r.rootTokens[normalizeRoot(title.repoPath)]; ok {
					spans = append(spans, redactionSpan{
						start: start, end: titleStart - 1, replacement: token, priority: spanKnownRoot,
					})
				}
				scan = end
				continue
			}
			scan = start + 1
		}
	}
	return spans
}

func (r *redactor) appendWorktreeSubdirectoryTitleSpans(spans []redactionSpan, s string) []redactionSpan {
	return r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, s, derivedWorktreePathBoundary)
}

func (r *redactor) appendWorktreeSubdirectoryTitleSpansWithBoundary(
	spans []redactionSpan,
	s string,
	boundary pathBoundary,
) []redactionSpan {
	if r.afHome == "" {
		return spans
	}
	for _, afHome := range r.afHomeSpellings {
		parent := filepath.Join(afHome, "worktrees")
		for segment := range r.worktreeSubdirectoryTitles {
			needle := filepath.Join(parent, segment)
			scan := 0
			for scan <= len(s)-len(needle) {
				rel := strings.Index(s[scan:], needle)
				if rel < 0 {
					break
				}
				start := scan + rel
				end := start + len(needle)
				if boundary(s, start, end) {
					spans = append(spans, redactionSpan{
						start: start + len(needle) - len(segment),
						end:   end, replacement: redactedMarker, priority: spanWorktreeTitle,
					})
					scan = end
					continue
				}
				scan = start + 1
			}
		}
	}
	return spans
}

func (r *redactor) appendKnownRootSpans(spans []redactionSpan, s string) []redactionSpan {
	return r.appendKnownRootSpansWithBoundary(spans, s, knownRootTextBoundary)
}

// appendLogOnlyPathBlankSpans blanks the verbatim repo paths the generic fallback
// gathered (noteUnknownJSONRecord → noteLogOnlyPathRedaction) to the marker in
// log and diagnostic text only — never in the generic/config arm, and never as a
// numbered root. The typed path collapses the same value to a root token via
// appendKnownRootSpans; #4115 deliberately registered no root here, so this is the
// fallback's substitute for the daemon log tail, preserving #4115's "no structural
// role" while closing #3588's "hold across every section at once" gap.
//
// Two contexts are blanked:
//
//   - The bare path as a complete value (e.g. repo_path="<path>"), matched at a
//     known-root text boundary so an unrelated prefix survives.
//   - The repo-path prefix of a proven sibling worktree path. appendWorktreePathTitleSpans
//     already redacts the registered title SEGMENT; this blanks the prefix that the
//     typed path's root-token span would have covered, so the verbatim path does
//     not survive beside its own redacted title segment.
//
// A path that IS a registered root (exactly) is left to appendKnownRootSpans and
// the worktree-title pass: the root collapse consumes the whole path as its
// token, which a marker over the same bytes would only destroy. A path merely
// UNDER a registered root is NOT deferred: the root collapse rewrites only the
// ancestor, leaving the private descendant leaf bare — the typed path would
// register that exact path as its own root, which the fallback declines per
// #4115, so the log-only blank must still fire to blank the verbatim value.
// $HOME is deliberately not a "registered root" for this test: collapsing $HOME
// to "~" rewrites only the prefix and leaves the private repo leaf, so an
// in-$HOME repo still needs the blank to keep its leaf out of the log.
func (r *redactor) appendLogOnlyPathBlankSpans(spans []redactionSpan, s string) []redactionSpan {
	return r.appendLogOnlyPathBlankSpansWithBoundary(spans, s, knownRootTextBoundary, derivedWorktreePathBoundary)
}

func (r *redactor) appendLogOnlyPathBlankSpansWithBoundary(
	spans []redactionSpan,
	s string,
	bareBoundary, siblingBoundary pathBoundary,
) []redactionSpan {
	if r.logOnlyPathBlanksSaturated {
		// Fail-closed: the registered set is capped, so the per-needle scan
		// cannot reach records registered past the cap anyway. Skip it and
		// blank every absolute-path token in this view in a single O(text)
		// pass, so a private path past the cap does not survive the daemon log
		// verbatim while the scan stays bounded by the text size, not the
		// rejected-record count.
		return r.appendSaturatedLogOnlyPathBlankSpans(spans, s)
	}
	if r.worktreePathTitlesSaturated {
		// Fail-closed for the sibling shape: noteFallbackWorktreeTitle dropped
		// past-cap (repo_path, title) pairs, so the per-needle sibling-prefix
		// loop and appendWorktreePathTitleSpans cannot reach a daemon-log
		// sibling spelling such as "<repo_path>-<past-cap-title>" — the
		// bare-path blank rejects the repo_path because it is immediately
		// followed by '-', and knownRootTextBoundary accepts that dash only
		// once the title pass has already replaced the suffix with -[redacted],
		// which cannot happen for a pair the cap dropped. Blank every
		// absolute-path token in this view so neither the verbatim repo path
		// nor the past-cap title segment survives the daemon log, mirroring the
		// path-cap fail-closed. The degenerate case that saturates the cap
		// (more than 4096 distinct rejected titles sharing one repo_path)
		// already forgoes the typed per-title layout, so erring toward the
		// marker preserves the privacy contract at the cost of layout only
		// that case ever had (#4938 review).
		return r.appendSaturatedLogOnlyPathBlankSpans(spans, s)
	}
	if len(r.logOnlyPathBlanks) == 0 {
		return spans
	}
	// Bare path: blank wherever it appears as a complete path value at a text
	// boundary. A registered root is not required; the value is blanked, not
	// collapsed, so no token grant is made. Skip only a path that IS a
	// registered root (exactly): the root collapse consumes the whole path as
	// its token. A path merely under a registered root is still blanked — the
	// root collapse would rewrite only the ancestor and strand the private
	// descendant leaf, and the typed path registers that exact path as its own
	// root, which the fallback declines per #4115. Also skip a path that is
	// exactly a registered worktree-title sibling needle (repo_path + "-" +
	// title segment): the worktree-title pass redacts the segment and the root
	// span or the sibling-prefix blank redacts the repo prefix, together
	// preserving the typed path's layout. This only defers the proven-sibling
	// shape; a malformed or non-sibling alternate_path is not a needle, so it
	// is still blanked here — which is the case the worktree-title machinery
	// cannot reach.
	for path := range r.logOnlyPathBlanks {
		if r.registeredRootEquals(path) {
			continue
		}
		if r.isWorktreeTitleSiblingNeedle(path) {
			continue
		}
		scan := 0
		for scan <= len(s)-len(path) {
			rel := strings.Index(s[scan:], path)
			if rel < 0 {
				break
			}
			start := scan + rel
			end := start + len(path)
			if bareBoundary(s, start, end) {
				spans = append(spans, redactionSpan{
					start: start, end: end, replacement: redactedMarker, priority: spanQuotedValue,
				})
				scan = end
				continue
			}
			scan = start + 1
		}
	}
	// Sibling prefix: a registered worktree-title needle proves the bytes after
	// the dash are title data appendWorktreePathTitleSpans is already redacting.
	// Blank the repo-path prefix the typed path would have collapsed to a root
	// token, so the verbatim path does not survive next to its redacted title.
	for title := range r.worktreePathTitles {
		if !r.logOnlyPathBlank(title.repoPath) {
			continue
		}
		// A registered root already owns this prefix — exactly — so
		// appendKnownRootSpans produces its token form there. Leaving the prefix
		// to the root pass keeps one redaction per byte range and preserves
		// layout. A repoPath merely under a registered root is NOT deferred: the
		// root collapse would rewrite only the ancestor and leave the private
		// descendant leaf beside the redacted title segment, so the
		// sibling-prefix blank must still fire.
		if r.registeredRootEquals(title.repoPath) {
			continue
		}
		needle := title.repoPath + "-" + title.segment
		scan := 0
		for scan <= len(s)-len(needle) {
			rel := strings.Index(s[scan:], needle)
			if rel < 0 {
				break
			}
			start := scan + rel
			end := start + len(needle)
			if siblingBoundary(s, start, end) {
				spans = append(spans, redactionSpan{
					start: start, end: start + len(title.repoPath),
					replacement: redactedMarker, priority: spanQuotedValue,
				})
				scan = end
				continue
			}
			scan = start + 1
		}
	}
	return spans
}

func (r *redactor) logOnlyPathBlank(path string) bool {
	_, ok := r.logOnlyPathBlanks[path]
	return ok
}

// appendSaturatedLogOnlyPathBlankSpans is the fail-closed scan the log-only
// path cap switches to once noteLogOnlyPathRedaction reaches maxLogOnlyPathBlanks.
// The capped per-needle scan cannot reach records registered past the cap, so
// dropping them would let a daemon-log tail line for an omitted record ship its
// private path verbatim (the fallback JSON redaction protects a separate
// section). This single O(text) pass blanks every path-shaped token at a
// path-text boundary instead, so no untracked path survives the log while the
// scan stays bounded by the text size.
//
// Relative as well as absolute paths are covered. NewGitWorktreeFromStorage
// only rejects empty paths, so a rejected record can carry a relative
// repo_path/worktree_path (e.g. "private-client/repo"); when the path cap is
// saturated the per-needle scan that registered that spelling no longer runs,
// so the fail-closed scan must reach it too. A relative path embedded mid-value
// doesn't open with '/', so each '/' the main loop finds walks backward through
// the previous path-legal bytes to the previous delimiter (or the start of the
// view); for an absolute path the byte before the '/' is already a delimiter
// (or the start of the view) so that walk is a no-op, and for a relative path
// it extends the blank to the path's first byte, the same way the leading-slash
// whole-value blank covers a path-valued scalar (#4938 review).
//
// It is deliberately more aggressive than the registered pass: a registered
// root that sits inside an untracked path also blanks here, because
// once the cap is saturated the redactor can no longer tell which paths
// are safe to keep — exactly the fail-closed trade #3588 makes across
// sections. The degenerate case that saturates the cap (more than 4096
// distinct spellings from a hand-edited or corrupted archive) already forgoes
// the typed path's per-root layout, so erring toward the marker preserves the
// privacy contract at the cost of layout only that case ever had. Non-absolute
// values that never carry a path separator (relative branch refs, agent
// names, status labels) are not touched, so triage outside the path bytes
// survives.
func (r *redactor) appendSaturatedLogOnlyPathBlankSpans(spans []redactionSpan, s string) []redactionSpan {
	i := 0
	for i < len(s) {
		if s[i] != '/' {
			i++
			continue
		}
		// Begin the blank at the start of the path-shaped token. Walk back
		// from this '/' through the previous path-legal bytes to the previous
		// path-text delimiter (or the start of the view): a relative path
		// embedded mid-value (recover_error="... private-client/repo ...")
		// doesn't open with '/', so the leading-slash whole-value blank below
		// doesn't reach it; for an absolute path the byte before the '/' is
		// already a delimiter (or the start of the view) so this walk is a
		// no-op. The walkthrough never crosses a '"': the saturated scan also
		// runs on whole-record views where a '"' is the structural terminator
		// of a %q value, and isPathTextDelimiter recognizes it (#4938 review).
		start := i
		for start > 0 {
			before, size := utf8.DecodeLastRuneInString(s[:start])
			if isPathTextDelimiter(before) {
				break
			}
			start -= size
		}
		end := start
		// The shared stage runs this fail-closed scan on a single decoded
		// value (ProvLogValue) as well as on a whole log record. When the
		// blank starts at the very first byte of the view, the whole view is
		// one %q-decoded path scalar — a relative path opened by the walk-back
		// above (e.g. "private-client/repo"), or an absolute path that opened
		// with '/' (the original i==0 case) — whose interior may carry
		// filename-legal bytes that isPathTextDelimiter treats as boundaries,
		// notably spaces, which Go's %q leaves literal (a value such as
		// `/srv/Acme Project/SecretRepo` is emitted as
		// `repo_path="/srv/Acme Project/SecretRepo"`). Scanning to the first
		// such delimiter would blank only the prefix and ship the private
		// suffix, so blank the entire decoded value instead of its first
		// delimiter-free token (#4938 review). A whole-record view that
		// walked back to its first byte still rare-cases this branch: a record
		// that opens with a path-shaped token rather than the emitter label is
		// already malformed, and fail-closed errs toward the marker; a prose
		// %q value such as recover_error never reaches here because the
		// walk-back stops at the leading delimiter before its first '/'.
		if start == 0 {
			end = len(s)
		} else {
			for end < len(s) {
				c, size := utf8.DecodeRuneInString(s[end:])
				if isPathTextDelimiter(c) {
					break
				}
				end += size
			}
			// A path embedded in a prose %q value (recover_error="recovery
			// location: /srv/Acme;Project/SecretRepo") never opens with '/',
			// so the i==0 whole-value blank above does not apply. The per-token
			// scan stops at the ';' inside "Acme;Project" and would blank only
			// "/srv/Acme", shipping the private "Project/SecretRepo" suffix —
			// WORKTREE_MISSING_DETECTED emits recover_error with %q in
			// daemon/lostrestore.go, so recovery messages are prose-valued
			// quoted fields, not path-valued scalars. On a Unix filesystem only
			// NUL (and the path separator '/', which isPathTextDelimiter
			// deliberately does not include) cannot appear inside a name, so
			// every other isPathTextDelimiter rune — space, tab, ';', ',',
			// ':', '=', single quote, '(', ')', '[', ']', '{', '}', '<', '>', '&',
			// '|', and '`' — is a filename-legal renderer terminator: a path
			// could legitimately contain it even though the renderer that
			// emitted the surrounding prose used the same byte as a separator.
			// The fail-closed saturated scan therefore extends the blank
			// through the path-with-delimiters run — path-legal bytes and any
			// filename-legal delimiter — until a real terminator (NUL, the end
			// of the decoded value, or a double quote '"' which closes a %q
			// field in a whole-record view this same scan also runs on).
			// '"' is excluded because the saturated scan cannot tell a literal
			// '"' inside a filename from the structural terminator of a %q
			// value, and crossing it would blank unrelated values from a
			// whole-record view, so erring toward the closing quote preserves
			// the privacy contract in the prose-value case without over-
			// blanking the record case. Repeated spaces are filename-legal
			// and do not end the run (#4938 review).
			if end < len(s) {
				if c, _ := utf8.DecodeRuneInString(s[end:]); c != '\x00' && c != '"' {
					end = saturatedPathContinuationEnd(s, end)
				}
			}
		}
		// A real path has at least two bytes (a lone "/" or adjacent-delimiter
		// "/" is not a path value); a relative path's walk-back produces
		// "ab/cd"-shaped spans so this still rejects the degenerate single-
		// byte case. knownRootTextBoundary reuses the same start/end boundary
		// the registered pass uses, so a relative branch ref glued to a word
		// byte and a path mid-token are not promoted to a blank.
		if end-start >= 2 && knownRootTextBoundary(s, start, end) {
			spans = append(spans, redactionSpan{
				start: start, end: end, replacement: redactedMarker, priority: spanQuotedValue,
			})
			i = end
			continue
		}
		i++
	}
	return spans
}

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
// filename-legal delimiter, ending only at NUL (the one byte that cannot
// appear in a Unix filename), a double quote '"' (the structural terminator
// of a %q value in a daemon log record, which the saturated scan must not
// cross when it runs on the whole record, since it cannot tell a literal '"'
// inside a filename from the value's closing quote), or the end of the
// decoded value. Repeated spaces are filename-legal and do not end the run:
// the prior single-space-only extension shipped the suffix of a path such as
// "/srv/Acme  Project/SecretRepo" between the two spaces. This is fail-closed:
// in the saturated case the redactor can no longer tell an embedded path
// from prose that merely looks like one, so erring toward the marker preserves
// the privacy contract at the cost of layout only that degenerate case ever
// had (#4938 review).
func saturatedPathContinuationEnd(s string, delimiterEnd int) int {
	end := delimiterEnd
	for end < len(s) {
		c, size := utf8.DecodeRuneInString(s[end:])
		if c == '\x00' || c == '"' {
			break
		}
		end += size
	}
	return end
}

// isWorktreeTitleSiblingNeedle reports whether path is exactly a registered
// worktree-title sibling needle — repo_path + "-" + the title segment
// noteWorktreeTitle stored. Such a path is redacted in pieces by the
// worktree-title pass (the segment) and the sibling-prefix blank or a
// registered root span (the repo_path prefix), which preserves the typed
// path's "[token]-[redacted]" layout (or "[redacted]-[redacted]" when no root is
// registered). A non-sibling or malformed-record alternate_path is not a
// needle, so it falls through to the bare blank instead of being deferred here.
func (r *redactor) isWorktreeTitleSiblingNeedle(path string) bool {
	for title := range r.worktreePathTitles {
		if title.repoPath+"-"+title.segment == path {
			return true
		}
	}
	return false
}

// registeredRootEquals reports whether a registered root (the AF home, a
// session's repo, or a worktree) names path EXACTLY. Such a path is left to
// appendKnownRootSpans and the worktree-title pass, which collapse the whole
// path to its token; a marker over the same bytes would only destroy it.
//
// A path merely UNDER a registered root is NOT equal and so is not deferred:
// the root collapse would rewrite only the ancestor and strand the private
// descendant leaf — the typed path would register that exact path as its own
// root, which the fallback declines per #4115 — so the log-only blank still
// fires to blank the verbatim value.
//
// $HOME is deliberately excluded: collapsing it to "~" rewrites only the
// prefix, leaving the private repo leaf, so an in-$HOME repo still needs the
// log-only blank (a typed path gets the same effect by registering the repo as
// a root, which the fallback declines per #4115).
func (r *redactor) registeredRootEquals(path string) bool {
	for _, root := range r.roots {
		if rest, ok := underRoot(path, root.path); ok && rest == "" {
			return true
		}
	}
	return false
}

func (r *redactor) appendKnownRootSpansWithBoundary(
	spans []redactionSpan,
	s string,
	boundary pathBoundary,
) []redactionSpan {
	for _, root := range r.rootReplacements() {
		scan := 0
		for scan <= len(s)-len(root.path) {
			rel := strings.Index(s[scan:], root.path)
			if rel < 0 {
				break
			}
			start := scan + rel
			end := start + len(root.path)
			if boundary(s, start, end) {
				spans = append(spans, redactionSpan{
					start: start, end: end, replacement: root.token, priority: spanKnownRoot,
				})
				scan = end
				continue
			}
			scan = start + 1
		}
	}
	return spans
}

func appendExactSpans(spans []redactionSpan, s, value, replacement string, priority int) []redactionSpan {
	if value == "" {
		return spans
	}
	for scan := 0; scan <= len(s)-len(value); {
		rel := strings.Index(s[scan:], value)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(value)
		if !insideRedactionMarker(s, start, end) {
			spans = append(spans, redactionSpan{
				start: start, end: end, replacement: replacement, priority: priority,
			})
		}
		scan = end
	}
	return spans
}

func appendTokenSpans(
	spans []redactionSpan,
	s, token, replacement string,
	isTokenRune func(rune) bool,
	priority int,
) []redactionSpan {
	if strings.TrimSpace(token) == "" || (!containsWordRune(token) && !strings.ContainsAny(token, "\r\n")) {
		return spans
	}
	for scan := 0; scan <= len(s)-len(token); {
		rel := strings.Index(s[scan:], token)
		if rel < 0 {
			break
		}
		start := scan + rel
		end := start + len(token)
		if tokenBoundary(s, start, end, isTokenRune) && !insideRedactionMarker(s, start, end) {
			spans = append(spans, redactionSpan{
				start: start, end: end, replacement: replacement, priority: priority,
			})
		}
		scan = start + 1
	}
	return spans
}

// applyRedactionSpans always covers the union of spans. Priority only chooses a
// semantic marker for equal-length matches.
func applyRedactionSpans(s string, spans []redactionSpan) string {
	shared := make([]redactspan.Span, 0, len(spans))
	for _, span := range spans {
		shared = append(shared, redactspan.Span{
			Start: span.start, End: span.end, Replacement: span.replacement, Priority: span.priority,
		})
	}
	return redactspan.Apply(s, shared, redactedMarker)
}
