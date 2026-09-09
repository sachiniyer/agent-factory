package bugreport

import (
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

var afLogRecordStart = regexp.MustCompile(
	`(?m)^(?:\[DAEMON\] )?(?:INFO|WARNING|ERROR):\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} [^:\r\n]+:\d+: `,
)

// appendLogShellCommandPathSpans recognizes post-worktree emitters that print a
// /bin/sh -c value: the legacy running line's raw final field, its current %q
// field, the other hook emitters' %q field, and the root-agent program's %q
// field. Literal AF prose proves the command's provenance; arbitrary output
// that merely looks shell-like never enters the shell parser.
func (r *redactor) appendLogShellCommandPathSpans(spans []redactionSpan, s string) []redactionSpan {
	const (
		runPrefix            = "running post-worktree hook in "
		quotedPrefix         = "post-worktree hook "
		rootAgentPrefix      = "ensured root agent for "
		rootAgentProgramOpen = " (in-place, program "
		outputOpen           = " (output: "
		commandSep           = "): "
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
		rootAgent := strings.Index(line, rootAgentPrefix)
		program := strings.LastIndex(line, rootAgentProgramOpen)
		if rootAgent >= 0 && program > rootAgent {
			quotedStart := lineStart + program + len(rootAgentProgramOpen)
			quotedEnd := goQuotedEnd(s[:contentEnd], quotedStart)
			if quotedEnd >= 0 && s[quotedEnd:contentEnd] == ")" {
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
					quotedEnd := -1
					if commandStart < contentEnd && s[commandStart] == '"' {
						quotedEnd = goQuotedEnd(s[:contentEnd], commandStart)
					}
					if quotedEnd == contentEnd {
						spans = r.appendLogShellQuotedSpan(spans, s, commandStart, contentEnd)
					} else {
						commandEnd := nextAFLogRecordStart(s, contentEnd)
						command := s[commandStart:commandEnd]
						for _, span := range r.shellCommandSpans(command, r.sensitiveTextSpans) {
							span.start += commandStart
							span.end += commandStart
							spans = append(spans, span)
						}
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

// nextAFLogRecordStart bounds the legacy raw %s hook field. A physical newline
// may belong to the shell command; only the exact prefix grammar configured by
// log.Initialize proves that a later line begins another record.
func nextAFLogRecordStart(s string, from int) int {
	if loc := afLogRecordStart.FindStringIndex(s[from:]); loc != nil {
		return from + loc[0]
	}
	return len(s)
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
	inner = append(inner, r.shellCommandSpans(value, r.sensitiveTextSpans)...)
	if redacted := applyRedactionSpans(value, inner); redacted != value {
		spans = append(spans, redactionSpan{
			start: start, end: end, replacement: strconv.Quote(redacted), priority: spanQuotedValue,
		})
	}
	return spans
}

func (r *redactor) shellCommandSpans(command string, produce textSpanProducer) []redactionSpan {
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
	spans = r.appendKnownRootSpansWithBoundary(spans, command, rootBoundary)
	return appendShellLiteralSpans(spans, context.literalRuns, produce)
}

type shellWordRange = textSourceRange

type shellPathContext struct {
	expansions        map[int][]shellWordRange
	pathnameExpansion map[int][]shellWordRange
	lineContinuations map[int][]shellWordRange
	literalRuns       []shellLiteralRun
	source            string
}

type shellLiteralRun = sourceMappedText

func parseShellPathContext(command string) (shellPathContext, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return shellPathContext{}, false
	}
	context := shellPathContext{
		expansions:        make(map[int][]shellWordRange),
		pathnameExpansion: make(map[int][]shellWordRange),
		lineContinuations: make(map[int][]shellWordRange),
		source:            command,
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		word, ok := node.(*syntax.Word)
		if !ok {
			return true
		}
		wordRange := shellWordRange{start: int(word.Pos().Offset()), end: int(word.End().Offset())}
		context.noteLiteralRuns(word)
		// Only top-level literal parts can carry active pathname expansion.
		// Literals nested below single/double quotes are deliberately excluded.
		for _, part := range word.Parts {
			lit, ok := part.(*syntax.Lit)
			if !ok {
				continue
			}
			context.notePathnameExpansions(command, lit, wordRange)
		}
		syntax.Walk(word, func(part syntax.Node) bool {
			switch part := part.(type) {
			case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
				start := int(part.Pos().Offset())
				context.expansions[start] = append(context.expansions[start], wordRange)
				return false
			case *syntax.Lit:
				context.noteLineContinuations(command, part, wordRange)
			}
			return true
		})
		// Keep walking so words nested inside a command substitution receive the
		// same grammar-owned treatment. noteLiteralRuns itself stops at the
		// substitution boundary in the outer word.
		return true
	})
	return context, true
}

func appendShellLiteralSpans(
	spans []redactionSpan,
	runs []shellLiteralRun,
	produce textSpanProducer,
) []redactionSpan {
	for _, run := range runs {
		spans = appendSourceMappedTextSpans(spans, run, produce)
	}
	return spans
}

// noteLiteralRuns models the transformations the POSIX shell applies to source
// bytes before a word can name a path. Adjacent quoted and unquoted literals
// concatenate; quote removal contributes no bytes; and escape removal maps one
// logical byte back to the complete source spelling that produced it. Dynamic
// parameter/command/arithmetic/process expansions (and their field splitting)
// plus pathname expansions split a run, because their execution-time bytes are
// absent from the report. Tilde expansion likewise materializes no private
// source bytes, so its literal '~' spelling needs no synthetic path span.
func (c *shellPathContext) noteLiteralRuns(word *syntax.Word) {
	builder := shellLiteralBuilder{source: c.source}
	for _, part := range word.Parts {
		builder.appendPart(part, false)
	}
	builder.flush()
	c.literalRuns = append(c.literalRuns, builder.runs...)
}

type shellLiteralBuilder struct {
	source  string
	value   []byte
	mapping []shellWordRange
	runs    []shellLiteralRun
}

func (b *shellLiteralBuilder) appendPart(part syntax.WordPart, doubleQuoted bool) {
	switch part := part.(type) {
	case *syntax.Lit:
		b.appendLiteral(int(part.Pos().Offset()), int(part.End().Offset()), doubleQuoted)
	case *syntax.SglQuoted:
		// Dollar-single quotes are outside the configured POSIX grammar. Should a
		// future parser variant admit them, do not guess their escape semantics.
		if part.Dollar {
			b.flush()
			return
		}
		b.appendVerbatim(int(part.Left.Offset())+1, int(part.Right.Offset()))
	case *syntax.DblQuoted:
		if part.Dollar {
			b.flush()
			return
		}
		for _, nested := range part.Parts {
			b.appendPart(nested, true)
		}
	default:
		// Expansions can generate, split, or glob execution-time fields, but
		// those generated bytes are not present to leak. They are a hard break
		// between the literal source runs we can prove and map.
		b.flush()
	}
}

func (b *shellLiteralBuilder) appendLiteral(start, end int, doubleQuoted bool) {
	if start < 0 || end > len(b.source) || start > end {
		b.flush()
		return
	}
	for offset := start; offset < end; {
		if b.source[offset] == '\\' && offset+1 < end {
			next := b.source[offset+1]
			if next == '\n' {
				offset += 2
				continue
			}
			if next == '\r' && offset+2 < end && b.source[offset+2] == '\n' {
				offset += 3
				continue
			}
			if !doubleQuoted || strings.ContainsRune("$`\"\\", rune(next)) {
				b.appendByte(next, offset, offset+2)
				offset += 2
				continue
			}
		}
		if !doubleQuoted && shellPathnameExpansionStartsAt(b.source, offset, end) {
			b.flush()
			offset++
			continue
		}
		b.appendByte(b.source[offset], offset, offset+1)
		offset++
	}
}

func (b *shellLiteralBuilder) appendVerbatim(start, end int) {
	if start < 0 || end > len(b.source) || start > end {
		b.flush()
		return
	}
	for offset := start; offset < end; offset++ {
		b.appendByte(b.source[offset], offset, offset+1)
	}
}

func (b *shellLiteralBuilder) appendByte(value byte, start, end int) {
	b.value = append(b.value, value)
	b.mapping = append(b.mapping, shellWordRange{start: start, end: end})
}

func (b *shellLiteralBuilder) flush() {
	if len(b.value) == 0 {
		return
	}
	b.runs = append(b.runs, shellLiteralRun{
		value: string(b.value), source: append([]shellWordRange(nil), b.mapping...),
	})
	b.value = b.value[:0]
	b.mapping = b.mapping[:0]
}

func shellPathnameExpansionStartsAt(command string, offset, end int) bool {
	switch command[offset] {
	case '*', '?':
		return true
	case '[':
		if pattern.HasMeta(command[offset:end], 0) {
			_, err := pattern.Regexp(command[offset:end], 0)
			return err == nil
		}
	}
	return false
}

func (c shellPathContext) expansionStartsAt(start, end int) bool {
	if c.directExpansionStartsAt(start, end) {
		return true
	}
	next, ok := c.afterLineContinuations(start, end)
	if !ok {
		return false
	}
	return pathEndsAt(c.source, start, next) || c.directExpansionStartsAt(start, next)
}

func (c shellPathContext) directExpansionStartsAt(start, end int) bool {
	words := append(c.expansions[end], c.pathnameExpansion[end]...)
	for _, word := range words {
		if start >= word.start && end <= word.end {
			return true
		}
	}
	return false
}

func (c shellPathContext) afterLineContinuations(start, end int) (int, bool) {
	next := end
	seen := false
	for {
		owned := false
		for _, word := range c.lineContinuations[next] {
			if start >= word.start && next < word.end {
				owned = true
				break
			}
		}
		if !owned {
			return next, seen
		}
		seen = true
		next += 2
		if c.source[next-1] == '\r' && next < len(c.source) && c.source[next] == '\n' {
			next++
		}
	}
}

func (c shellPathContext) notePathnameExpansions(command string, lit *syntax.Lit, word shellWordRange) {
	start, end := int(lit.Pos().Offset()), int(lit.End().Offset())
	for offset := start; offset < end; offset++ {
		if command[offset] == '\\' {
			offset++
			continue
		}
		if shellPathnameExpansionStartsAt(command, offset, end) {
			c.pathnameExpansion[offset] = append(c.pathnameExpansion[offset], word)
		}
	}
}

func (c shellPathContext) noteLineContinuations(command string, lit *syntax.Lit, word shellWordRange) {
	start, end := int(lit.Pos().Offset()), int(lit.End().Offset())
	for offset := start; offset+1 < end; offset++ {
		if command[offset] != '\\' {
			continue
		}
		if command[offset+1] == '\n' ||
			(command[offset+1] == '\r' && offset+2 < end && command[offset+2] == '\n') {
			c.lineContinuations[offset] = append(c.lineContinuations[offset], word)
		}
	}
}
