// Package redactspan applies independently discovered sensitive intervals to
// their shared original input.
package redactspan

import (
	"sort"
	"strings"
)

// Span replaces one half-open byte interval in the original input. Priority is
// used only to choose between equal-length semantic replacements.
type Span struct {
	Start       int
	End         int
	Replacement string
	Priority    int
}

// Apply replaces the union of valid spans. Longer non-overlapping spans retain
// their semantic replacement; any portion of an overlapping span that they do
// not cover is replaced with fallback. No accepted span can be discarded in a
// way that exposes its otherwise-uncovered source bytes.
func Apply(input string, spans []Span, fallback string) string {
	if len(spans) == 0 {
		return input
	}
	sort.SliceStable(spans, func(i, j int) bool {
		iLen := spans[i].End - spans[i].Start
		jLen := spans[j].End - spans[j].Start
		if iLen != jLen {
			return iLen > jLen
		}
		if spans[i].Priority != spans[j].Priority {
			return spans[i].Priority < spans[j].Priority
		}
		return spans[i].Start < spans[j].Start
	})

	selected := make([]Span, 0, len(spans))
	covered := make([]bool, len(input))
	for _, candidate := range spans {
		if candidate.Start < 0 || candidate.End <= candidate.Start || candidate.End > len(input) {
			continue
		}
		if intervalUncovered(covered, candidate.Start, candidate.End) {
			selected = append(selected, candidate)
			cover(covered, candidate.Start, candidate.End)
			continue
		}
		for next := candidate.Start; next < candidate.End; {
			for next < candidate.End && covered[next] {
				next++
			}
			start := next
			for next < candidate.End && !covered[next] {
				next++
			}
			if start < next {
				selected = append(selected, Span{Start: start, End: next, Replacement: fallback})
				cover(covered, start, next)
			}
		}
	}
	if len(selected) == 0 {
		return input
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Start < selected[j].Start })

	var out strings.Builder
	copied := 0
	for _, span := range selected {
		out.WriteString(input[copied:span.Start])
		out.WriteString(span.Replacement)
		copied = span.End
	}
	out.WriteString(input[copied:])
	return out.String()
}

func intervalUncovered(covered []bool, start, end int) bool {
	for i := start; i < end; i++ {
		if covered[i] {
			return false
		}
	}
	return true
}

func cover(covered []bool, start, end int) {
	for i := start; i < end; i++ {
		covered[i] = true
	}
}
