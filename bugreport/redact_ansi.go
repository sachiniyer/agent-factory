package bugreport

import (
	xansi "github.com/charmbracelet/x/ansi"
)

// ansiTextContext records complete terminal control sequences in daemon-log
// provenance. Hook output may wrap a filesystem path in zero-width styling;
// parsing those controls keeps them out of the path without treating a bare ESC
// byte as a delimiter in ordinary text.
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

func (r *redactor) appendANSITransformedSpans(
	spans []redactionSpan,
	s string,
	produce textSpanProducer,
) []redactionSpan {
	context := parseANSITextContext(s)
	if len(context.starts) == 0 {
		return spans
	}
	logical := context.sourceMappedText(s)
	spans = appendSourceMappedTextSpans(spans, logical, produce)
	for start, sequence := range context.starts {
		if sequence.payload == "" {
			continue
		}
		inner := r.sensitiveTextSpans(sequence.payload)
		if applyRedactionSpans(sequence.payload, inner) == sequence.payload {
			continue
		}
		spans = append(spans, redactionSpan{
			start: start, end: sequence.end,
			replacement: redactedMarker, priority: spanQuotedValue,
		})
	}
	return spans
}

func (c ansiTextContext) sourceMappedText(s string) sourceMappedText {
	value := make([]byte, 0, len(s))
	source := make([]textSourceRange, 0, len(s))
	for offset := 0; offset < len(s); {
		if sequence, ok := c.starts[offset]; ok {
			offset = sequence.end
			continue
		}
		value = append(value, s[offset])
		source = append(source, textSourceRange{start: offset, end: offset + 1})
		offset++
	}
	return sourceMappedText{value: string(value), source: source}
}
