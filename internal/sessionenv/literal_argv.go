package sessionenv

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// LiteralCommandArgv reduces a command string to its argv only when it is one
// simple call of literal words: no assignment prefix, no expansion, no
// redirection, no operator, no second command. It shares singleSimpleCall with
// the account launch proof, so every caller parses command strings through
// the same grammar rather than owning a second copy.
//
// The word walk is deliberately more precise than the proof's
// literalShellWord (Codex review on #5487): literalShellWord emits a
// syntax.Lit part's raw bytes, so an unquoted `*` or `~` on disk would be
// reported as the literal word `*` or `~` even though the shell expands it at
// exec time. This walker DECODES unquoted backslash escapes per POSIX (\x is
// the literal character x, and backslash-newline is removed — the same decode
// that makes ShellQuotePath's '\” apostrophe idiom round-trip) and then
// refuses any word whose UNQUOTED, UNESCAPED characters the shell could still
// act on: a glob metacharacter, a tilde in expansion position, or a brace
// expansion. Quoted or escaped copies of the same characters are literal text
// and stay comparable.
func LiteralCommandArgv(command string) ([]string, bool) {
	call, ok := singleSimpleCall(command)
	if !ok || len(call.Assigns) > 0 {
		return nil, false
	}
	args := make([]string, len(call.Args))
	for i, word := range call.Args {
		text, ok := literalArgvWord(word)
		if !ok {
			return nil, false
		}
		args[i] = text
	}
	return args, true
}

// LiteralCommandsEqual reports whether two command strings run the same argv:
// the same literal words in the same order, with nothing resolved or
// normalized — a bare `claude` does not match `/abs/path/claude`, and flag
// order is not canonicalized. Quoting or escaping a word differently is not a
// difference; an added, removed, reordered, or expandable word is. A command
// that is not a plain literal word list is not equal to anything — including
// itself — so a trust decision fails closed and a drift check falls back to
// comparing bytes.
func LiteralCommandsEqual(a, b string) bool {
	aArgv, ok := LiteralCommandArgv(a)
	if !ok {
		return false
	}
	bArgv, ok := LiteralCommandArgv(b)
	if !ok {
		return false
	}
	return slices.Equal(aArgv, bArgv)
}

// literalArgvWord renders one word to its literal text. Quoted parts
// contribute their text verbatim; unquoted Lit parts are decoded per POSIX
// (\x emits literal x, \<newline> emits nothing) and simultaneously checked
// for syntax the shell could still act on — an UNESCAPED glob (* ? [), an
// unescaped tilde in expansion position (the word's first character, or
// directly after an unescaped = or : — the assignment-form positions bash
// expands even in argument position), or an unescaped $ immediately before a
// quote part (the POSIX spelling of $'…'/$"…", literal to a POSIX sh but
// decoded input to bash). unquoted accumulates only unescaped Lit characters
// for the brace check, since every separator of a brace expansion must be
// unquoted and unescaped.
func literalArgvWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var value, unquoted strings.Builder
	// prev/prevInert track the last emitted byte and whether it came from an
	// unquoted, unescaped source — the only context that can put a following
	// tilde in expansion position.
	prev, prevInert := byte(0), false
	for i, part := range word.Parts {
		if dq, ok := part.(*syntax.DblQuoted); ok {
			before := value.Len()
			if !appendLiteralDblQuoted(&value, dq) {
				return "", false
			}
			if value.Len() > before {
				prev, prevInert = value.String()[value.Len()-1], false
			}
			continue
		}
		lit, ok := part.(*syntax.Lit)
		if !ok {
			before := value.Len()
			if !appendLiteralShellPart(&value, part) {
				return "", false
			}
			if value.Len() > before {
				prev, prevInert = value.String()[value.Len()-1], false
			}
			continue
		}
		raw := lit.Value
		for j := 0; j < len(raw); j++ {
			c := raw[j]
			if c == '\\' {
				if j+1 == len(raw) {
					// Unreachable from a successful parse (a trailing escape
					// continues into the next part); keep it as literal text.
					value.WriteByte('\\')
					prev, prevInert = '\\', false
					continue
				}
				n := raw[j+1]
				if n == '\n' {
					j++
					continue
				}
				value.WriteByte(n)
				prev, prevInert = n, false
				j++
				continue
			}
			switch c {
			case '*', '?', '[':
				return "", false
			case '~':
				if (i == 0 && j == 0) || (prevInert && (prev == '=' || prev == ':')) {
					return "", false
				}
			}
			value.WriteByte(c)
			unquoted.WriteByte(c)
			prev, prevInert = c, true
		}
		// The $ check runs after the Lit is decoded so an escaped \$ — literal
		// text — is not mistaken for the $'…'/$"…" spelling.
		if prevInert && prev == '$' && i+1 < len(word.Parts) && isQuotedWordPart(word.Parts[i+1]) {
			return "", false
		}
	}
	if braceExpandable(unquoted.String()) {
		return "", false
	}
	return value.String(), true
}

// appendLiteralDblQuoted renders the text inside "…" with the POSIX
// double-quote decode: backslash escapes only $ ` " \ and newline, and before
// any other character it is literal. A nested Lit's raw Value therefore
// cannot be copied verbatim — "a\$b" and 'a\$b' pass different bytes to exec
// (Codex review round 3). A part that is not a Lit is an expansion — $x,
// $(…), `…`, or an arithmetic form — and makes the whole word non-literal.
// Escaped and quoted characters inside here never reach the caller's unquoted
// skeleton, so they stay inert for the glob, tilde, and brace scans.
func appendLiteralDblQuoted(b *strings.Builder, dq *syntax.DblQuoted) bool {
	if dq == nil || dq.Dollar {
		return false
	}
	for _, part := range dq.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			return false
		}
		raw := lit.Value
		for j := 0; j < len(raw); j++ {
			c := raw[j]
			if c == '\\' && j+1 < len(raw) {
				switch raw[j+1] {
				case '$', '`', '"', '\\':
					b.WriteByte(raw[j+1])
					j++
					continue
				case '\n':
					j++
					continue
				}
			}
			b.WriteByte(c)
		}
	}
	return true
}

func isQuotedWordPart(part syntax.WordPart) bool {
	switch part.(type) {
	case *syntax.SglQuoted, *syntax.DblQuoted:
		return true
	}
	return false
}

// braceExpandable reports whether the concatenated unescaped, unquoted text
// of a word carries a brace expansion. Bash pairs braces at any depth — {a,b}
// and {x..y} each produce several words — so every unquoted { is checked
// against every later unquoted }: if the span between them holds an unquoted
// , or .. the word expands. That rule over-approximates bash's (a non-nested
// separator inside a span whose outer braces do not pair can still be
// flagged), which is the conservative direction: it can refuse a literal word
// but never certify an expandable one. Escaped and quoted characters never
// reach this text, so a{','}b stays literal while a{'x',y}b expands — exactly
// like the shells.
func braceExpandable(unquoted string) bool {
	for i := 0; i < len(unquoted); i++ {
		if unquoted[i] != '{' {
			continue
		}
		for j := i + 1; j < len(unquoted); j++ {
			if unquoted[j] != '}' {
				continue
			}
			span := unquoted[i+1 : j]
			if strings.Contains(span, ",") || strings.Contains(span, "..") {
				return true
			}
		}
	}
	return false
}
