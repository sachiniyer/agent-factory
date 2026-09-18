package redactx

import (
	"strconv"
	"strings"
)

// maxGoQuotedTransformDepth bounds nested %q decoding. Each accepted quote
// removes at least its two delimiter bytes, but an attacker-controlled log
// can still manufacture excessive nesting; at the budget any further valid
// quoted value is redacted as one unknown logical unit rather than leaked or
// recursed without a bound.
const maxGoQuotedTransformDepth = 64

// GoQuotedEnd reports the end offset of the Go-quoted string opening at
// start, or -1 if none closes it. Exported for consumers that must find the
// same token boundary the transform does (tests, custom re-encoders).
func GoQuotedEnd(s string, start int) int {
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

// goQuoteTransform decodes each valid "..." token in the text and, when the
// decoded value scrubs to something different, rewrites the whole source
// token with the re-encoded scrubbed value. This is the re-encoding half of
// the closed set: the replacement must be valid syntax in the grammar it
// came from, so the transform owns the strconv.Quote boundary rather than
// mapping inner matches back byte-by-byte.
type goQuoteTransform struct{}

func (goQuoteTransform) name() string { return "go-quote" }

func (goQuoteTransform) admit(prov Provenance) bool {
	return provIn(prov,
		ProvLogRecord, ProvLogValue, ProvLogShell, ProvDiagnostic, ProvGeneric)
}

func (goQuoteTransform) trigger(text string) bool { return hasByte('"')(text) }

// decodedProvenance is the context a %q-decoded value re-enters under: a
// decoded field inside a log record or shell command is a log value; every
// other provenance carries through unchanged.
func decodedProvenance(prov Provenance) Provenance {
	if prov == ProvLogRecord || prov == ProvLogShell {
		return ProvLogValue
	}
	return prov
}

func (t goQuoteTransform) decode(e *Engine, text string, prov Provenance, depth int) transformResult {
	var res transformResult
	childProv := decodedProvenance(prov)
	atDepthBudget := depth >= maxGoQuotedTransformDepth
	for scan := 0; scan < len(text); {
		rel := strings.IndexByte(text[scan:], '"')
		if rel < 0 {
			break
		}
		start := scan + rel
		end := GoQuotedEnd(text, start)
		if end < 0 {
			scan = start + 1
			continue
		}
		quoted := text[start:end]
		value, err := strconv.Unquote(quoted)
		if err != nil {
			// This opener did not establish a Go-quoted value. Resume one byte past
			// it: the quote we tentatively treated as its closer may instead be the
			// next real %q opener, and malformed text must not consume that evidence.
			scan = start + 1
			continue
		}
		if atDepthBudget {
			res.rewrites = append(res.rewrites, rewrite{
				r:    Range{Start: start, End: end},
				repl: strconv.Quote(e.FailClosed.Replacement),
			})
		} else if redacted := e.scrubText(value, childProv, depth+1); redacted != value {
			res.rewrites = append(res.rewrites, rewrite{
				r:    Range{Start: start, End: end},
				repl: strconv.Quote(redacted),
			})
		}
		scan = end
	}
	return res
}
