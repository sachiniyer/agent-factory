package bugreport

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/credscrub"
	"github.com/sachiniyer/agent-factory/internal/redactspan"
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

// scrubKnownLogValues includes the historical task-log shapes whose title is
// known from fixed syntax rather than the current record set.
func (r *redactor) scrubKnownLogValues(s string) string {
	spans := r.sensitiveTextSpans(s)
	spans = r.appendANSIPathSpans(spans, s)
	spans = appendLegacyTaskTitleSpans(spans, s)
	spans = r.appendLogShellCommandPathSpans(spans, s)
	spans = r.appendQuotedValueSpans(spans, s, true)
	return applyRedactionSpans(s, spans)
}

func (r *redactor) scrubKnownDiagnosticValues(s string) string {
	spans := r.sensitiveTextSpans(s)
	spans = r.appendANSIPathSpans(spans, s)
	spans = r.appendQuotedValueSpans(spans, s, false)
	return applyRedactionSpans(s, spans)
}

func (r *redactor) scrubGenericText(s string) string {
	spans := r.genericTextSpans(s)
	spans = r.appendGenericQuotedValueSpans(spans, s)
	return applyRedactionSpans(s, spans)
}

func (r *redactor) sensitiveTextSpans(s string) []redactionSpan {
	spans := r.knownTextSpans(s)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

func (r *redactor) genericTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendAccountLabelSpans(spans, s)
	spans = r.appendWorktreePathTitleSpans(spans, s)
	spans = r.appendWorktreeSubdirectoryTitleSpans(spans, s)
	spans = r.appendKnownRootSpans(spans, s)
	spans = r.appendURIPathSpans(spans, s)
	spans = appendCredentialSpans(spans, s)
	return r.appendUsernameSpans(spans, s)
}

// knownTextSpans resolves every contextual/name/path match against one original
// string. Priority only chooses a semantic marker for equal-length matches;
// applyRedactionSpans always covers the union.
func (r *redactor) knownTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendKnownLabelSpans(spans, s)
	spans = r.appendTmuxNameSpans(spans, s)
	spans = r.appendWorktreePathTitleSpans(spans, s)
	spans = r.appendWorktreeSubdirectoryTitleSpans(spans, s)
	spans = r.appendKnownRootSpans(spans, s)
	spans = r.appendURIPathSpans(spans, s)
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

func (r *redactor) appendQuotedValueSpans(spans []redactionSpan, s string, legacyLog bool) []redactionSpan {
	return appendGoQuotedSpans(spans, s, func(value string) string {
		inner := r.sensitiveTextSpans(value)
		if legacyLog {
			inner = appendLegacyTaskTitleSpans(inner, value)
		}
		return applyRedactionSpans(value, inner)
	})
}

func (r *redactor) appendGenericQuotedValueSpans(spans []redactionSpan, s string) []redactionSpan {
	return appendGoQuotedSpans(spans, s, func(value string) string {
		return applyRedactionSpans(value, r.genericTextSpans(value))
	})
}

func appendGoQuotedSpans(spans []redactionSpan, s string, scrub func(string) string) []redactionSpan {
	for scan := 0; scan < len(s); {
		rel := strings.IndexByte(s[scan:], '"')
		if rel < 0 {
			break
		}
		start := scan + rel
		end := goQuotedEnd(s, start)
		if end < 0 {
			scan = start + 1
			continue
		}
		quoted := s[start:end]
		value, err := strconv.Unquote(quoted)
		if err != nil {
			// This opener did not establish a Go-quoted value. Resume one byte past
			// it: the quote we tentatively treated as its closer may instead be the
			// next real %q opener, and malformed text must not consume that evidence.
			scan = start + 1
			continue
		}
		if redacted := scrub(value); redacted != value {
			spans = append(spans, redactionSpan{
				start: start, end: end, replacement: strconv.Quote(redacted), priority: spanQuotedValue,
			})
		}
		scan = end
	}
	return spans
}

func goQuotedEnd(s string, start int) int {
	escaped := false
	for i := start + 1; i < len(s); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch s[i] {
		case '\\':
			escaped = true
		case '"':
			return i + 1
		case '\r', '\n':
			// Go double-quoted literals cannot contain a physical newline. Log
			// framing therefore terminates this malformed candidate before a quote
			// on the next daemon line can be mistaken for its closer.
			return -1
		}
	}
	return -1
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

func applyRedactionSpans(s string, spans []redactionSpan) string {
	shared := make([]redactspan.Span, 0, len(spans))
	for _, span := range spans {
		shared = append(shared, redactspan.Span{
			Start: span.start, End: span.end, Replacement: span.replacement, Priority: span.priority,
		})
	}
	return redactspan.Apply(s, shared, redactedMarker)
}
