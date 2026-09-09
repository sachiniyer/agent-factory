package bugreport

import (
	"path/filepath"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
)

// ansiTextContext records complete terminal control sequences in daemon-log
// provenance. Hook output may wrap a filesystem path in zero-width styling;
// parsing those controls keeps them out of the path without treating a bare ESC
// byte as a delimiter in ordinary text.
type ansiTextContext struct {
	starts map[int]int
	ends   map[int]int
}

func parseANSITextContext(s string) ansiTextContext {
	context := ansiTextContext{starts: make(map[int]int), ends: make(map[int]int)}
	state := byte(0)
	for offset := 0; offset < len(s); {
		sequence, width, n, nextState := xansi.DecodeSequence(s[offset:], state, nil)
		if n <= 0 {
			break
		}
		if state == 0 && nextState == 0 && width == 0 && len(sequence) > 1 && sequence[0] == '\x1b' {
			context.starts[offset] = offset + n
			context.ends[offset+n] = offset
		}
		offset += n
		state = nextState
	}
	return context
}

func (r *redactor) appendANSIPathSpans(spans []redactionSpan, s string) []redactionSpan {
	context := parseANSITextContext(s)
	if len(context.starts) == 0 {
		return spans
	}
	worktreeBoundary := func(s string, start, end int) bool {
		return derivedWorktreePathBoundaryWithContext(s, start, end, context.pathStartsAt, context.pathEndsAt)
	}
	rootBoundary := func(s string, start, end int) bool {
		return knownRootTextBoundaryWithContext(s, start, end, context.pathStartsAt, context.pathEndsAt)
	}
	spans = r.appendWorktreePathTitleSpansWithBoundary(spans, s, worktreeBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, s, worktreeBoundary)
	return r.appendKnownRootSpansWithBoundary(spans, s, rootBoundary)
}

func (c ansiTextContext) pathStartsAt(s string, start int) bool {
	for {
		before, ok := c.ends[start]
		if !ok {
			return pathStartsAt(s, start)
		}
		start = before
	}
}

func (c ansiTextContext) pathEndsAt(s string, start, end int) bool {
	originalEnd := end
	for {
		after, ok := c.starts[end]
		if !ok {
			break
		}
		end = after
	}
	if end == originalEnd {
		return pathEndsAt(s, start, end)
	}
	if end == len(s) || s[end] == byte(filepath.Separator) {
		return true
	}
	after, _ := utf8.DecodeRuneInString(s[end:])
	return isPathTextDelimiter(after)
}
