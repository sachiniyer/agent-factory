package bugreport

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// redactionSpan is one replacement located in the unmodified input. Log and
// diagnostic redaction has several independently useful views of the same text:
// a title can contain a root, a root can contain a title, and a tmux name can
// contain either. Recording all matches first keeps one matcher from destroying
// the evidence another needs.
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
	spanUsername
)

// scrubKnownLogValues includes the historical task-log shapes whose title is
// known from fixed syntax rather than the current record set.
func (r *redactor) scrubKnownLogValues(s string) string {
	spans := r.knownTextSpans(s)
	spans = appendLegacyTaskTitleSpans(spans, s)
	return applyRedactionSpans(s, spans)
}

func (r *redactor) scrubKnownDiagnosticValues(s string) string {
	return applyRedactionSpans(s, r.knownTextSpans(s))
}

// knownTextSpans resolves every contextual/name/path match against one original
// string. Priority only breaks equal-length ties; a longer match always wins.
func (r *redactor) knownTextSpans(s string) []redactionSpan {
	spans := make([]redactionSpan, 0)
	spans = r.appendKnownLabelSpans(spans, s)
	spans = r.appendTmuxNameSpans(spans, s)
	spans = r.appendWorktreePathTitleSpans(spans, s)
	spans = r.appendWorktreeSubdirectoryTitleSpans(spans, s)
	spans = r.appendKnownRootSpans(spans, s)
	for _, name := range r.users {
		spans = appendTokenSpans(spans, s, name, userMarker, isWordRune, spanUsername)
	}
	return spans
}

func (r *redactor) appendKnownLabelSpans(spans []redactionSpan, s string) []redactionSpan {
	for label := range r.accounts {
		spans = appendTokenSpans(spans, s, label, redactedMarker, isAccountNameRune, spanKnownLabel)
	}
	for title := range r.titles {
		quoted := strconv.Quote(title)
		spans = appendExactSpans(spans, s, quoted, strconv.Quote(redactedMarker), spanQuotedTitle)
		spans = appendTokenSpans(spans, s, title, redactedMarker, isWordRune, spanKnownLabel)
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
			if derivedWorktreePathBoundary(s, start, end) {
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
	if r.afHome == "" {
		return spans
	}
	parent := filepath.Join(r.afHome, "worktrees")
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
			if derivedWorktreePathBoundary(s, start, end) {
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
	return spans
}

func (r *redactor) appendKnownRootSpans(spans []redactionSpan, s string) []redactionSpan {
	for _, root := range r.rootReplacements() {
		scan := 0
		for scan <= len(s)-len(root.path) {
			rel := strings.Index(s[scan:], root.path)
			if rel < 0 {
				break
			}
			start := scan + rel
			end := start + len(root.path)
			if knownRootTextBoundary(s, start, end) {
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
	if len(spans) == 0 {
		return s
	}
	sort.SliceStable(spans, func(i, j int) bool {
		iLen := spans[i].end - spans[i].start
		jLen := spans[j].end - spans[j].start
		if iLen != jLen {
			return iLen > jLen
		}
		if spans[i].priority != spans[j].priority {
			return spans[i].priority < spans[j].priority
		}
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return false
	})

	selected := make([]redactionSpan, 0, len(spans))
	occupied := make([]bool, len(s))
	for _, candidate := range spans {
		if candidate.start < 0 || candidate.end <= candidate.start || candidate.end > len(s) {
			continue
		}
		overlaps := false
		for i := candidate.start; i < candidate.end; i++ {
			if occupied[i] {
				overlaps = true
				break
			}
		}
		if !overlaps {
			selected = append(selected, candidate)
			for i := candidate.start; i < candidate.end; i++ {
				occupied[i] = true
			}
		}
	}
	if len(selected) == 0 {
		return s
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].start < selected[j].start })

	var out strings.Builder
	copied := 0
	for _, span := range selected {
		out.WriteString(s[copied:span.start])
		out.WriteString(span.replacement)
		copied = span.end
	}
	out.WriteString(s[copied:])
	return out.String()
}
