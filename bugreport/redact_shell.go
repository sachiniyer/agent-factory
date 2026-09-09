package bugreport

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// appendLogShellCommandPathSpans recognizes post-worktree emitters that print a
// /bin/sh -c value: the running line's raw final field and the other emitters'
// %q field. Literal AF prose proves the command's provenance; arbitrary output
// that merely looks shell-like never enters the shell parser. log.Printf line
// framing owns the raw command's end.
func (r *redactor) appendLogShellCommandPathSpans(spans []redactionSpan, s string) []redactionSpan {
	const (
		runPrefix    = "running post-worktree hook in "
		quotedPrefix = "post-worktree hook "
		outputOpen   = " (output: "
		commandSep   = "): "
	)
	for lineStart := 0; lineStart < len(s); {
		lineEnd := strings.IndexByte(s[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(s)
		} else {
			lineEnd += lineStart
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && s[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := s[lineStart:contentEnd]
		quoted := strings.Index(line, quotedPrefix)
		if quoted >= 0 {
			quotedStart := lineStart + quoted + len(quotedPrefix)
			if quotedStart < contentEnd && s[quotedStart] == '"' {
				spans = r.appendLogShellQuotedSpan(spans, s, quotedStart, contentEnd)
			}
		}
		run := strings.Index(line, runPrefix)
		if run >= 0 {
			afterRun := run + len(runPrefix)
			output := strings.Index(line[afterRun:], outputOpen)
			if output >= 0 {
				afterOutput := afterRun + output + len(outputOpen)
				separator := strings.Index(line[afterOutput:], commandSep)
				if separator >= 0 {
					commandStart := lineStart + afterOutput + separator + len(commandSep)
					command := s[commandStart:contentEnd]
					for _, span := range r.shellCommandPathSpans(command) {
						span.start += commandStart
						span.end += commandStart
						spans = append(spans, span)
					}
				}
			}
		}
		if lineEnd == len(s) {
			break
		}
		lineStart = lineEnd + 1
	}
	return spans
}

func (r *redactor) appendLogShellQuotedSpan(
	spans []redactionSpan,
	s string,
	start, lineEnd int,
) []redactionSpan {
	end := goQuotedEnd(s[:lineEnd], start)
	if end < 0 {
		return spans
	}
	value, err := strconv.Unquote(s[start:end])
	if err != nil {
		return spans
	}
	inner := r.sensitiveTextSpans(value)
	inner = appendLegacyTaskTitleSpans(inner, value)
	inner = append(inner, r.shellCommandPathSpans(value)...)
	if redacted := applyRedactionSpans(value, inner); redacted != value {
		spans = append(spans, redactionSpan{
			start: start, end: end, replacement: strconv.Quote(redacted), priority: spanQuotedValue,
		})
	}
	return spans
}

func (r *redactor) shellCommandPathSpans(command string) []redactionSpan {
	context, ok := parseShellPathContext(command)
	if !ok {
		// Proven shell provenance with syntax our parser cannot establish is an
		// unknown logical value, not permission to fall back to plain-text path
		// boundaries. Fail closed over the command while leaving surrounding log
		// prose or config structure intact.
		if command == "" {
			return nil
		}
		return []redactionSpan{{
			start: 0, end: len(command), replacement: redactedMarker, priority: spanQuotedValue,
		}}
	}
	endsAt := func(s string, start, end int) bool {
		return pathEndsAt(s, start, end) || context.expansionStartsAt(start, end)
	}
	worktreeBoundary := func(s string, start, end int) bool {
		return derivedWorktreePathBoundaryWithEnd(s, start, end, endsAt)
	}
	rootBoundary := func(s string, start, end int) bool {
		return knownRootTextBoundaryWithEnd(s, start, end, endsAt)
	}
	spans := r.appendWorktreePathTitleSpansWithBoundary(nil, command, worktreeBoundary)
	spans = r.appendWorktreeSubdirectoryTitleSpansWithBoundary(spans, command, worktreeBoundary)
	return r.appendKnownRootSpansWithBoundary(spans, command, rootBoundary)
}

type shellWordRange struct {
	start int
	end   int
}

type shellPathContext struct {
	expansions map[int][]shellWordRange
}

func parseShellPathContext(command string) (shellPathContext, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return shellPathContext{}, false
	}
	context := shellPathContext{expansions: make(map[int][]shellWordRange)}
	syntax.Walk(file, func(node syntax.Node) bool {
		word, ok := node.(*syntax.Word)
		if !ok {
			return true
		}
		wordRange := shellWordRange{start: int(word.Pos().Offset()), end: int(word.End().Offset())}
		syntax.Walk(word, func(part syntax.Node) bool {
			switch part.(type) {
			case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
				start := int(part.Pos().Offset())
				context.expansions[start] = append(context.expansions[start], wordRange)
				return false
			}
			return true
		})
		return false
	})
	return context, true
}

func (c shellPathContext) expansionStartsAt(start, end int) bool {
	for _, word := range c.expansions[end] {
		if start >= word.start && end <= word.end {
			return true
		}
	}
	return false
}
