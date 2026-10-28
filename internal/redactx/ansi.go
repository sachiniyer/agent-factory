package redactx

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// ansiTextContext records complete terminal control sequences in
// log/diagnostic provenance. Hook output may wrap a path in zero-width
// styling; parsing those controls keeps them out of the path without
// treating a bare ESC byte as a delimiter in ordinary text.
type ansiTextContext struct {
	starts map[int]ansiTextSequence
}

type ansiTextSequence struct {
	end     int
	payload string
}

func parseANSITextContext(s string) ansiTextContext {
	context := ansiTextContext{starts: make(map[int]ansiTextSequence)}
	parser := xansi.NewParser()
	if len(s) > 0 {
		parser.SetDataSize(len(s))
	}
	state := byte(0)
	for offset := 0; offset < len(s); {
		sequence, width, n, nextState := xansi.DecodeSequence(s[offset:], state, parser)
		if n <= 0 {
			break
		}
		if state == 0 && nextState == 0 && width == 0 && len(sequence) > 1 &&
			isANSISequenceIntroducer(sequence[0]) {
			context.starts[offset] = ansiTextSequence{
				end: offset + n, payload: string(parser.Data()),
			}
		}
		offset += n
		state = nextState
	}
	return context
}

func isANSISequenceIntroducer(b byte) bool {
	switch b {
	case '\x1b', xansi.DCS, xansi.SOS, xansi.CSI, xansi.OSC, xansi.PM, xansi.APC:
		return true
	default:
		return false
	}
}

// ansiTrigger gates the ANSI parse on the bytes that can introduce a control
// sequence: ESC or one of the C1 single-byte introducers. A line without any
// of them — the common case on the log path — skips the parser entirely.
func ansiTrigger(text string) bool {
	return strings.IndexAny(text, "\x1b\x90\x98\x9b\x9d\x9e\x9f") >= 0
}

// ansiTransform removes complete ANSI controls from display text, making a
// sequence inserted mid-token zero-width. Its decoded view re-enters the full
// stage under the same provenance, so a %q value or URI inside the stripped
// text still decodes. String-control payloads are a separate nested channel:
// one containing a producer match replaces the COMPLETE control rather than
// preserving an unsafe opaque payload.
type ansiTransform struct{}

func (ansiTransform) name() string { return "ansi" }

func (ansiTransform) admit(prov Provenance) bool {
	return provIn(prov, ProvLogRecord, ProvLogValue, ProvLogShell, ProvDiagnostic)
}

func (ansiTransform) trigger(text string) bool { return ansiTrigger(text) }

func (ansiTransform) decode(e *Engine, text string, prov Provenance, depth int) transformResult {
	context := parseANSITextContext(text)
	if len(context.starts) == 0 {
		return transformResult{}
	}
	var res transformResult
	res.views = append(res.views, viewOut{view: context.sourceMappedText(text), prov: prov})
	for start, sequence := range context.starts {
		if sequence.payload == "" {
			continue
		}
		if !e.probe(sequence.payload, ProvANSIPayload, depth+1) {
			continue
		}
		res.fail = append(res.fail, Range{Start: start, End: sequence.end})
	}
	return res
}

func (c ansiTextContext) sourceMappedText(s string) View {
	value := make([]byte, 0, len(s))
	source := make([]Range, 0, len(s))
	for offset := 0; offset < len(s); {
		if sequence, ok := c.starts[offset]; ok {
			offset = sequence.end
			continue
		}
		value = append(value, s[offset])
		source = append(source, Range{Start: offset, End: offset + 1})
		offset++
	}
	return View{Text: string(value), Source: source}
}
