package redactx

import (
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// afLogRecordStart bounds the legacy raw %s hook field. A physical newline
// may belong to the shell command; only the exact prefix grammar configured
// by log.Initialize proves that a later line begins another record.
var afLogRecordStart = regexp.MustCompile(
	`(?m)^(?:\[DAEMON\] )?(?:INFO|WARNING|ERROR):\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} [^:\r\n]+:\d+: `,
)

// logShellEmitter recognizes post-worktree emitters that print a /bin/sh -c
// value: the legacy running line's raw final field, its current %q field, the
// other hook emitters' %q field, and the root-agent program's %q field.
// Literal AF prose proves the command's provenance; arbitrary output that
// merely looks shell-like never enters the shell parser.
//
// It is a transform, not a producer: the command bytes it isolates become
// ProvLogShell views (or rewrites for %q-carried commands), so every consumer
// of the stage gets shell dequoting on emitter lines without knowing AF's log
// format.
type logShellEmitter struct{}

// emitterForm selects how the command bytes follow an emitter prefix.
type emitterForm int

const (
	// emitterQuoted: the prefix is followed directly by a %q token.
	emitterQuoted emitterForm = iota
	// emitterRootAgentProgram: the %q token follows " (in-place, program "
	// later on the same line, closing with ")".
	emitterRootAgentProgram
	// emitterRawTail: the command follows " (output: <path>): " — a %q token
	// reaching end of line, else the raw tail bounded by the next AF record.
	emitterRawTail
)

// logEmitters is the one list both decode and trigger range over, so a new
// emitter is gated by construction: trigger(text) is true exactly when some
// prefix decode dispatches on is present. Keeping them one table is what makes
// "trigger false ⟹ decode finds nothing" structural rather than a wording
// coincidence (#4149 review).
var logEmitters = []struct {
	prefix string
	form   emitterForm
}{
	{prefix: "post-worktree hook ", form: emitterQuoted},
	{prefix: "ensured root agent for ", form: emitterRootAgentProgram},
	{prefix: "running post-worktree hook in ", form: emitterRawTail},
}

func (logShellEmitter) name() string { return "log-shell-emitter" }

func (logShellEmitter) admit(prov Provenance) bool { return prov == ProvLogRecord }

// trigger fires iff some emitter prefix decode dispatches on appears in the
// text — derived from the same table, so the two cannot drift.
func (logShellEmitter) trigger(text string) bool {
	for _, emitter := range logEmitters {
		if strings.Contains(text, emitter.prefix) {
			return true
		}
	}
	return false
}

func (logShellEmitter) decode(e *Engine, text string, _ Provenance, depth int) transformResult {
	const (
		rootAgentProgramOpen = " (in-place, program "
		outputOpen           = " (output: "
		commandSep           = "): "
	)
	var res transformResult
	for lineStart := 0; lineStart < len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += lineStart
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && text[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := text[lineStart:contentEnd]
		for _, emitter := range logEmitters {
			prefixAt := strings.Index(line, emitter.prefix)
			if prefixAt < 0 {
				continue
			}
			switch emitter.form {
			case emitterQuoted:
				quotedStart := lineStart + prefixAt + len(emitter.prefix)
				if quotedStart < contentEnd && text[quotedStart] == '"' {
					res.rewrites = append(res.rewrites,
						shellQuotedRewrite(e, text, quotedStart, contentEnd, depth)...)
				}
			case emitterRootAgentProgram:
				program := strings.LastIndex(line, rootAgentProgramOpen)
				if program <= prefixAt {
					continue
				}
				quotedStart := lineStart + program + len(rootAgentProgramOpen)
				quotedEnd := GoQuotedEnd(text[:contentEnd], quotedStart)
				if quotedEnd >= 0 && text[quotedEnd:contentEnd] == ")" {
					res.rewrites = append(res.rewrites,
						shellQuotedRewrite(e, text, quotedStart, contentEnd, depth)...)
				}
			case emitterRawTail:
				afterRun := prefixAt + len(emitter.prefix)
				output := strings.Index(line[afterRun:], outputOpen)
				if output < 0 {
					continue
				}
				afterOutput := afterRun + output + len(outputOpen)
				separator := strings.Index(line[afterOutput:], commandSep)
				if separator < 0 {
					continue
				}
				commandStart := lineStart + afterOutput + separator + len(commandSep)
				quotedEnd := -1
				if commandStart < contentEnd && text[commandStart] == '"' {
					quotedEnd = GoQuotedEnd(text[:contentEnd], commandStart)
				}
				if quotedEnd == contentEnd {
					res.rewrites = append(res.rewrites,
						shellQuotedRewrite(e, text, commandStart, contentEnd, depth)...)
				} else {
					commandEnd := nextAFLogRecordStart(text, contentEnd)
					res.views = append(res.views, viewOut{
						view: View{
							Text:   text[commandStart:commandEnd],
							Source: identityRange(commandStart, commandEnd),
						},
						prov: ProvLogShellRaw,
					})
				}
			}
		}
		if lineEnd == len(text) {
			break
		}
		lineStart = lineEnd + 1
	}
	return res
}

// shellQuotedRewrite decodes the %q token at [start, end) and rewrites it to
// the re-encoded scrub of its value — the value carrying shell provenance, so
// the returned text has already had literal-run matching applied.
func shellQuotedRewrite(e *Engine, text string, start, end, depth int) []rewrite {
	tokenEnd := GoQuotedEnd(text[:end], start)
	if tokenEnd < 0 {
		return nil
	}
	value, err := strconv.Unquote(text[start:tokenEnd])
	if err != nil {
		return nil
	}
	// Decoding %q establishes both a log-value region and, through the fixed
	// emitter prose above, its shell provenance. Re-entering one stage with
	// both facts keeps ANSI removal and every other admitted view available
	// on the logical command.
	redacted := e.scrubText(value, ProvLogShell, depth+1)
	if redacted == value {
		return nil
	}
	return []rewrite{{
		r:    Range{Start: start, End: tokenEnd},
		repl: strconv.Quote(redacted),
	}}
}

func identityRange(start, end int) []Range {
	source := make([]Range, end-start)
	for i := range source {
		source[i] = Range{Start: start + i, End: start + i + 1}
	}
	return source
}

// nextAFLogRecordStart bounds the legacy raw %s hook field. Only the exact
// prefix grammar configured by log.Initialize proves that a later line begins
// another record.
func nextAFLogRecordStart(s string, from int) int {
	if loc := afLogRecordStart.FindStringIndex(s[from:]); loc != nil {
		return from + loc[0]
	}
	return len(s)
}

// shellLiteralTransform parses a proven shell command into its literal runs:
// the byte sequences the shell would pass through untouched after quote
// removal, escape removal, and adjacent-literal concatenation. Each run is a
// view whose matches map back over the complete source spelling. Dynamic
// expansions, pathname expansions, and line continuations split runs because
// their execution-time bytes are absent.
//
// A command the parser cannot establish is not plain text to fall back on —
// it fails closed over the whole command.
type shellLiteralTransform struct{}

func (shellLiteralTransform) name() string { return "shell-literal" }

func (shellLiteralTransform) admit(prov Provenance) bool {
	return provIn(prov, ProvLogShell, ProvLogShellRaw, ProvConfigShell)
}

func (shellLiteralTransform) trigger(string) bool { return true }

func (t shellLiteralTransform) decode(_ *Engine, text string, prov Provenance, _ int) transformResult {
	context, ok := ParseShell(text)
	if !ok {
		if text == "" {
			return transformResult{}
		}
		return transformResult{fail: []Range{{Start: 0, End: len(text)}}}
	}
	literalProv := ProvLogShellLiteral
	if prov == ProvConfigShell {
		literalProv = ProvConfigShellLiteral
	}
	var res transformResult
	for _, run := range context.literalRuns {
		res.views = append(res.views, viewOut{view: run, prov: literalProv})
	}
	return res
}

// ShellContext is a parsed shell command: the literal runs plus the expansion
// positions a consumer's boundary predicates need to know where a path can
// end. Obtained via ParseShell; the zero value is unusable.
type ShellContext struct {
	expansions        map[int][]Range
	pathnameExpansion map[int][]Range
	lineContinuations map[int][]Range
	literalRuns       []View
	source            string
}

// ParseShell parses command under the POSIX grammar and models the
// transformations the shell applies to source bytes before a word can name a
// path. It reports false when the parser cannot establish the command's
// syntax — which is fail-closed territory, not permission to treat the text
// as unquoted.
func ParseShell(command string) (ShellContext, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return ShellContext{}, false
	}
	context := ShellContext{
		expansions:        make(map[int][]Range),
		pathnameExpansion: make(map[int][]Range),
		lineContinuations: make(map[int][]Range),
		source:            command,
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		word, ok := node.(*syntax.Word)
		if !ok {
			return true
		}
		wordRange := Range{Start: int(word.Pos().Offset()), End: int(word.End().Offset())}
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

// noteLiteralRuns models the transformations the POSIX shell applies to source
// bytes before a word can name a path. Adjacent quoted and unquoted literals
// concatenate; quote removal contributes no bytes; and escape removal maps one
// logical byte back to the complete source spelling that produced it. Dynamic
// parameter/command/arithmetic/process expansions (and their field splitting)
// plus pathname expansions split a run, because their execution-time bytes are
// absent from the report. Tilde expansion likewise materializes no private
// source bytes, so its literal '~' spelling needs no synthetic path span.
func (c *ShellContext) noteLiteralRuns(word *syntax.Word) {
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
	mapping []Range
	runs    []View
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
	b.mapping = append(b.mapping, Range{Start: start, End: end})
}

func (b *shellLiteralBuilder) flush() {
	if len(b.value) == 0 {
		return
	}
	b.runs = append(b.runs, View{
		Text: string(b.value), Source: append([]Range(nil), b.mapping...),
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

// DirectExpansionStartsAt reports whether a candidate path ending at
// [start,end) abuts a shell expansion whose materialized bytes are not
// source-mappable — used by consumer boundary predicates so a registered path
// may end where an expansion begins.
func (c ShellContext) DirectExpansionStartsAt(start, end int) bool {
	words := append(c.expansions[end], c.pathnameExpansion[end]...)
	for _, word := range words {
		if start >= word.Start && end <= word.End {
			return true
		}
	}
	return false
}

// AfterLineContinuations walks past any word-owned backslash-newline (or
// backslash-CRLF) continuations beginning at end, so a consumer boundary
// check can look past a physical continuation the shell erases. The second
// return reports whether at least one continuation was crossed.
func (c ShellContext) AfterLineContinuations(start, end int) (int, bool) {
	next := end
	seen := false
	for {
		owned := false
		for _, word := range c.lineContinuations[next] {
			if start >= word.Start && next < word.End {
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

func (c *ShellContext) notePathnameExpansions(command string, lit *syntax.Lit, word Range) {
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

func (c *ShellContext) noteLineContinuations(command string, lit *syntax.Lit, word Range) {
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
