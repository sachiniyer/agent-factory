package redactx

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/redactspan"
)

// Producer finds sensitive spans in one logical view's text. It is the only
// piece a consumer supplies: what counts as secret — credential shapes, a
// named token field, registered titles and roots — stays each scrubber's own
// policy. The engine owns which decodings are legal; the producer owns what
// is worth finding in the decoded result.
//
// Spans are returned in the view's own coordinates; the engine maps them back
// to source. A producer may return nil for a provenance it does not cover —
// the decoded view is then inert for that consumer.
type Producer func(text string, prov Provenance) []redactspan.Span

// Engine is one consumer's share of the normalization stage: a match policy
// plus the marker stamped where a transform could not decode a proven
// candidate. Build it once and reuse it; it holds no per-run state.
type Engine struct {
	// Produce is the consumer's matcher, run on every admitted view.
	Produce Producer
	// FailClosed supplies the Replacement and Priority stamped on two kinds
	// of span: a range a transform proved belongs to an encoding but could
	// not decode (a malformed escape, an unparseable shell command, a
	// payload that must not ship opaque), and the re-encoding rewrite a
	// transform emits for a token whose decoded value changed. Under a
	// redactor the ambiguous direction is always toward removing more.
	FailClosed redactspan.Span
	// Fallback is redactspan.Apply's marker for the uncovered portion of an
	// overlap, used by Scrub.
	Fallback string
}

// MatchText runs the full stage on s under prov: the producer's spans on s
// itself, unioned with its spans on every decoded view, mapped back to s.
func (e *Engine) MatchText(s string, prov Provenance) []redactspan.Span {
	return e.matchView(Identity(s), prov, 0)
}

// Scrub applies MatchText's union over s. It is also the entry point for a
// decoded scalar a consumer extracted itself — the value to re-encode into
// the owning grammar.
func (e *Engine) Scrub(s string, prov Provenance) string {
	return redactspan.Apply(s, e.MatchText(s, prov), e.Fallback)
}

// scrubText is the recursion a re-encoding transform calls on a decoded
// value: match it fully, apply in value coordinates, hand back the string to
// re-encode into the owning grammar.
func (e *Engine) scrubText(text string, prov Provenance, depth int) string {
	return redactspan.Apply(text, e.matchView(Identity(text), prov, depth), e.Fallback)
}

// matchText produces spans in text coordinates for a value that exists only
// as decoded text (a transform's inner recursion), as opposed to matchView
// which projects a source-mapped view.
func (e *Engine) matchText(text string, prov Provenance, depth int) []redactspan.Span {
	return e.matchView(Identity(text), prov, depth)
}

// probe reports whether the stage finds anything in text under prov.
// Transforms use it for fail-close decisions that hinge on "contains a
// match", like an ANSI payload that may not ship when it names a secret.
func (e *Engine) probe(text string, prov Provenance, depth int) bool {
	return len(e.matchText(text, prov, depth)) > 0
}

// maxTransformDepth is the whole-stage recursion backstop. Every admitted
// view is a strict substring or strict decode of its parent, so depth is
// already bounded by input length; this cap exists for pathological inputs
// that stack transforms faster than they shrink. It is deliberately looser
// than the %q transform's own budget (maxGoQuotedTransformDepth), which is
// the operative cap for the one transform family with a documented
// fail-closed behavior at its limit.
const maxTransformDepth = 128

func (e *Engine) matchView(v View, prov Provenance, depth int) []redactspan.Span {
	var spans []redactspan.Span
	spans = e.appendMappedSpans(spans, v, e.Produce(v.Text, prov))
	if depth >= maxTransformDepth {
		return spans
	}
	for _, t := range transforms {
		if !t.admit(prov) || !t.trigger(v.Text) {
			continue
		}
		res := t.decode(e, v.Text, prov, depth)
		for _, rg := range res.fail {
			if r, ok := v.MapSpan(rg.Start, rg.End); ok {
				spans = append(spans, redactspan.Span{
					Start: r.Start, End: r.End,
					Replacement: e.FailClosed.Replacement,
					Priority:    e.FailClosed.Priority,
				})
			}
		}
		for _, rw := range res.rewrites {
			if r, ok := v.MapSpan(rw.r.Start, rw.r.End); ok {
				spans = append(spans, redactspan.Span{
					Start: r.Start, End: r.End,
					Replacement: rw.repl,
					Priority:    e.FailClosed.Priority,
				})
			}
		}
		for _, w := range res.views {
			composed := compose(v, w.view)
			if len(composed.Source) != len(composed.Text) {
				continue
			}
			spans = append(spans, e.matchView(composed, w.prov, depth+1)...)
		}
	}
	return spans
}

// appendMappedSpans projects producer spans — which are in view coordinates —
// back through the view's source map into original-input coordinates.
func (e *Engine) appendMappedSpans(spans []redactspan.Span, v View, inner []redactspan.Span) []redactspan.Span {
	for _, sp := range inner {
		if r, ok := v.MapSpan(sp.Start, sp.End); ok {
			sp.Start, sp.End = r.Start, r.End
			spans = append(spans, sp)
		}
	}
	return spans
}

// transformResult is what one transform finds in one view's text. All ranges
// index into the text it was given; the engine composes them up.
type transformResult struct {
	// views are decoded logical values to recurse into, each carrying the
	// provenance its decoded content earned.
	views []viewOut
	// fail marks ranges that belong to a proven encoding but could not be
	// decoded. The engine redacts them whole — this is the contract that
	// makes "return best-effort" unrepresentable: a transform must say what
	// it could not decode, and what it cannot decode cannot ship.
	fail []Range
	// rewrites replace a whole source token with a re-encoded form of its
	// scrubbed value, for grammars (Go %q) whose safe replacement must be
	// valid syntax in the target encoding.
	rewrites []rewrite
}

type viewOut struct {
	view View
	prov Provenance
}

type rewrite struct {
	r    Range
	repl string
}

// transform is one admitted decoding in the closed set. admit gates on
// provenance; trigger is a cheap byte-level gate that keeps the transform
// free on text that cannot contain its encoding — the property that lets the
// stage run on the log hot path without paying a decode per line. A transform
// with no cheaper gate returns true.
type transform interface {
	name() string
	admit(Provenance) bool
	trigger(text string) bool
	decode(e *Engine, text string, prov Provenance, depth int) transformResult
}

// transforms is the registry — the closed transformation set as executable
// order. Ordering matters only where two transforms can rewrite the same
// bytes: the emitter-proven shell command precedes the generic %q pass so a
// hook command's quoted token keeps its shell-aware treatment, matching the
// pre-extraction order in bugreport.
var transforms = []transform{
	logShellEmitter{},
	ansiTransform{},
	goQuoteTransform{},
	uriTransform{},
	shellLiteralTransform{},
}

// provIn reports membership in a provenance set.
func provIn(prov Provenance, set ...Provenance) bool {
	for _, p := range set {
		if p == prov {
			return true
		}
	}
	return false
}

// hasByte is the common trigger shape: a transform that cannot fire unless a
// particular byte is present.
func hasByte(b byte) func(string) bool {
	return func(text string) bool {
		return strings.IndexByte(text, b) >= 0
	}
}
