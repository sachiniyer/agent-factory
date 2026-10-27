package bugreport

import (
	"path/filepath"
	"strconv"
	"strings"

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
	return r.appendKnownRootSpansWithBoundary(spans, s, uriKnownRootBoundary)
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
