// Package redactx is the shared text-transformation stage every AF secret
// scrubber runs matches through. It owns the closed set of encodings a
// registered or shape-matched secret must still be recognized under —
// declared in provenance.go — and the machinery that decodes each one into a
// logical view while keeping a byte-for-byte map back to the source text.
//
// The package exists because three redaction implementations — credscrub on
// the log path, agentproto on URLs, and the bugreport bundle scrubber — each
// grew their own decoder and kept leaking the same defect six times (#4149):
// a secret survived under an encoding its matcher did not normalize. Fixing
// one matcher about one encoding did not propagate. Here, adding a transform
// closes it once for every consumer.
//
// The model: text arrives with a Provenance saying which grammars may apply.
// The Engine runs the consumer's matcher on the text and on every logical
// view the admitted transforms produce, recursively (a %q field inside a log
// line can itself contain ANSI controls around a URI). Matches map back to
// source byte ranges, so replacement always rewrites the original text, and
// a candidate a transform cannot decode yields a fail-closed range instead
// of a best-effort guess.
package redactx

// Range is a half-open byte interval. In a View it locates the source bytes
// that produced one logical byte; in results it locates text to replace.
type Range struct {
	Start int
	End   int
}

// View is logical text produced by a transform, with Source[i] naming the
// source byte range that produced Text[i]. A transform may collapse several
// source bytes into one logical byte (an escape) or drop them (a control
// sequence contributes no bytes), but it never invents logical bytes without
// a source range, because a match can only be redacted where it came from.
type View struct {
	Text   string
	Source []Range
}

// Identity returns text as its own view: every byte its own source.
func Identity(text string) View {
	source := make([]Range, len(text))
	for i := range source {
		source[i] = Range{Start: i, End: i + 1}
	}
	return View{Text: text, Source: source}
}

// MapSpan projects a half-open span in Text coordinates back to the source
// range covering it: the start of the first byte's source through the end of
// the last byte's source, so a replacement covers the complete encoded
// spelling. It reports false for an out-of-bounds or empty span.
func (v View) MapSpan(start, end int) (Range, bool) {
	if start < 0 || end > len(v.Source) || start >= end || len(v.Source) != len(v.Text) {
		return Range{}, false
	}
	first, last := v.Source[start], v.Source[end-1]
	if first.Start < 0 || last.End < first.Start {
		return Range{}, false
	}
	return Range{Start: first.Start, End: last.End}, true
}

// compose folds a decoded view of a view into one step: inner indexes into
// outer.Text, so the composed view indexes straight into outer's source.
func compose(outer, inner View) View {
	if len(inner.Source) != len(inner.Text) {
		return View{}
	}
	source := make([]Range, len(inner.Source))
	for i, r := range inner.Source {
		mapped, ok := outer.MapSpan(r.Start, r.End)
		if !ok {
			mapped = Range{Start: -1, End: -1}
		}
		source[i] = mapped
	}
	return View{Text: inner.Text, Source: source}
}
