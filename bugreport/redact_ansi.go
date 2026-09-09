package bugreport

import (
	xansi "github.com/charmbracelet/x/ansi"
)

// ansiTextContext records complete terminal control sequences in daemon-log
// provenance. Hook output may wrap a filesystem path in zero-width styling;
// parsing those controls keeps them out of the path without treating a bare ESC
// byte as a delimiter in ordinary text.
type ansiTextContext struct {
	starts map[int]int
}

func parseANSITextContext(s string) ansiTextContext {
	context := ansiTextContext{starts: make(map[int]int)}
	state := byte(0)
	for offset := 0; offset < len(s); {
		sequence, width, n, nextState := xansi.DecodeSequence(s[offset:], state, nil)
		if n <= 0 {
			break
		}
		if state == 0 && nextState == 0 && width == 0 && len(sequence) > 1 &&
			isANSISequenceIntroducer(sequence[0]) {
			context.starts[offset] = offset + n
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

func (r *redactor) appendANSIPathSpans(spans []redactionSpan, s string) []redactionSpan {
	context := parseANSITextContext(s)
	if len(context.starts) == 0 {
		return spans
	}
	logical := context.sourceMappedText(s)
	spans = r.appendSourceMappedPathSpans(
		spans,
		logical,
		derivedWorktreePathBoundary,
		knownRootTextBoundary,
	)
	return r.appendSourceMappedURIPathSpans(spans, logical)
}

func (c ansiTextContext) sourceMappedText(s string) sourceMappedText {
	value := make([]byte, 0, len(s))
	source := make([]textSourceRange, 0, len(s))
	for offset := 0; offset < len(s); {
		if after, ok := c.starts[offset]; ok {
			offset = after
			continue
		}
		value = append(value, s[offset])
		source = append(source, textSourceRange{start: offset, end: offset + 1})
		offset++
	}
	return sourceMappedText{value: string(value), source: source}
}
