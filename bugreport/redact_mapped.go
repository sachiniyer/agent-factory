package bugreport

// textSourceRange records the source bytes that produced one logical byte.
// Recognized grammars may remove syntax or decode several source bytes into one
// value byte; path matching happens on the value and replacements are mapped
// back over the complete source spelling.
type textSourceRange struct {
	start int
	end   int
}

type sourceMappedText struct {
	value  string
	source []textSourceRange
}

func identitySourceMappedText(s string) sourceMappedText {
	source := make([]textSourceRange, len(s))
	for i := range s {
		source[i] = textSourceRange{start: i, end: i + 1}
	}
	return sourceMappedText{value: s, source: source}
}

func (r *redactor) appendSourceMappedPathSpans(
	spans []redactionSpan,
	text sourceMappedText,
	worktreeBoundary pathBoundary,
	rootBoundary pathBoundary,
) []redactionSpan {
	if len(text.value) == 0 || len(text.source) != len(text.value) {
		return spans
	}
	inner := r.appendWorktreePathTitleSpansWithBoundary(nil, text.value, worktreeBoundary)
	inner = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(inner, text.value, worktreeBoundary)
	inner = r.appendKnownRootSpansWithBoundary(inner, text.value, rootBoundary)
	for _, span := range inner {
		if span.start < 0 || span.end <= span.start || span.end > len(text.source) {
			continue
		}
		span.start = text.source[span.start].start
		span.end = text.source[span.end-1].end
		spans = append(spans, span)
	}
	return spans
}
