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
		text, bare, ok := literalArgvWord(word)
		if !ok || (i == 0 && bare && shellKeywords[text]) {
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

// shellKeywords are quote-sensitive in command position on hosts where
// /bin/sh is bash: `time claude` is pipeline syntax while 'time' claude is an
// executable lookup, so the two spellings are NOT the same command even
// though both reduce to the same argv (Codex review round 4). POSIX-reserved
// words like `if` or `{` never reach this check — they parse as clauses or
// fail outright — but bash-only keywords survive a POSIX parse as plain
// command words, so they are listed here alongside the rest for completeness.
// Only the command word (argv[0]) is checked, spelled entirely unquoted and
// unescaped; any quoting — including an empty ” — makes it a normal word.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"case": true, "esac": true, "for": true, "select": true, "while": true,
	"until": true, "do": true, "done": true, "in": true, "function": true,
	"time": true, "coproc": true, "{": true, "}": true, "!": true,
	"[[": true, "]]": true,
}

// literalArgvWord renders one word to its literal text plus whether the whole
// word arrived bare — every byte unquoted and unescaped — which the caller
// needs for the command-word keyword check. Quoted parts contribute their
// text verbatim; unquoted Lit parts are decoded per POSIX (\x emits literal
// x, \<newline> emits nothing) and simultaneously checked for syntax the
// shell could still act on — an UNESCAPED glob (* ? [), an unescaped tilde in
// expansion position (the word's first character, or directly after an
// unescaped = or : — the assignment-form positions bash expands even in
// argument position), or an unescaped $ immediately before a quote part (the
// POSIX spelling of $'…'/$"…", literal to a POSIX sh but decoded input to
// bash). unquoted accumulates only unescaped Lit characters for the brace
// check, since every separator of a brace expansion must be unquoted and
// unescaped.
func literalArgvWord(word *syntax.Word) (string, bool, bool) {
	if word == nil {
		return "", false, false
	}
	var value, unquoted strings.Builder
	// prev/prevInert track the last emitted byte and whether it came from an
	// unquoted, unescaped source — the only context that can put a following
	// tilde in expansion position.
	prev, prevInert := byte(0), false
	// wordStart stays true while nothing but line continuations has preceded
	// the current position: a continuation vanishes before tilde-prefix
	// evaluation, so a ~ behind one is still the word's first character. Any
	// emitted byte or quote part — even an empty one — ends it.
	wordStart := true
	bare := true
	// Tilde expansion after = or : applies only inside an assignment-shaped
	// word (NAME=...): a ~ in /tmp/a:~/claude is literal in every shell while
	// x=a:~ expands. assignment turns true when the word emits an unquoted =
	// whose decoded prefix is a valid shell name; lastDelim marks the position
	// right after that delimiter, since only the FIRST = enables a following ~
	// (v=a=~ is literal under bash).
	assignment, lastDelim := false, false
	for i, part := range word.Parts {
		if dq, ok := part.(*syntax.DblQuoted); ok {
			wordStart, bare, lastDelim = false, false, false
			before := value.Len()
			if !appendLiteralDblQuoted(&value, dq) {
				return "", false, false
			}
			if value.Len() > before {
				prev, prevInert = value.String()[value.Len()-1], false
			}
			continue
		}
		lit, ok := part.(*syntax.Lit)
		if !ok {
			wordStart, bare, lastDelim = false, false, false
			before := value.Len()
			if !appendLiteralShellPart(&value, part) {
				return "", false, false
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
				bare = false
				if j+1 == len(raw) {
					// Unreachable from a successful parse (a trailing escape
					// continues into the next part); keep it as literal text.
					value.WriteByte('\\')
					prev, prevInert, wordStart, lastDelim = '\\', false, false, false
					continue
				}
				n := raw[j+1]
				if n == '\n' {
					// A line continuation is removed before everything except
					// reserved-word recognition, which still sees the joined
					// word — the comparator cannot prove how ti\<newline>me
					// lexes, so it must not certify (Codex review round 6).
					return "", false, false
				}
				value.WriteByte(n)
				prev, prevInert, wordStart, lastDelim = n, false, false, false
				j++
				continue
			}
			switch c {
			case '*', '?', '[':
				return "", false, false
			case '~':
				if wordStart || lastDelim || (prevInert && prev == ':' && assignment) {
					return "", false, false
				}
			}
			delim := c == '=' && !assignment && isShellName(value.String())
			value.WriteByte(c)
			unquoted.WriteByte(c)
			prev, prevInert, wordStart, lastDelim = c, true, false, delim
			if delim {
				assignment = true
			}
		}
		// The $ check runs after the Lit is decoded so an escaped \$ — literal
		// text — is not mistaken for the $'…'/$"…" spelling.
		if prevInert && prev == '$' && i+1 < len(word.Parts) && isQuotedWordPart(word.Parts[i+1]) {
			return "", false, false
		}
	}
	if braceExpandable(unquoted.String()) {
		return "", false, false
	}
	return value.String(), bare, true
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
				if raw[j+1] == '\n' {
					// Same continuation ambiguity as the unquoted walk —
					// refuse rather than model it.
					return false
				}
				switch raw[j+1] {
				case '$', '`', '"', '\\':
					b.WriteByte(raw[j+1])
					j++
					continue
				}
			}
			b.WriteByte(c)
		}
	}
	return true
}

// isShellName reports whether s is a valid shell variable name — the
// assignment-shape gate for the = / : tilde rule.
func isShellName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		name := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !name {
			return false
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
// and {x..y} each produce several words — so this is a single pass over a
// stack of open braces (Codex review round 4: the earlier every-pair rescan
// was cubic in the command length). A , or .. at any depth marks its
// innermost enclosing frame, and a } that closes a marked frame means the
// word expands. Bash's pairing rule (verified against bash and dash): at
// depth 1 an unmarked } does NOT close the group — it is absorbed as literal
// text and the { keeps seeking a close, so {a},b} expands to "a}" "b" while
// {a},{b} stays literal because the inner { keeps the outer one from ever
// closing. A } deeper than depth 1 closes its nested group normally.
// Escaped and quoted characters never reach this text, so a{','}b stays
// literal while a{'x',y}b expands — exactly like the shells.
func braceExpandable(unquoted string) bool {
	var stack []bool
	for j := 0; j < len(unquoted); j++ {
		switch unquoted[j] {
		case '{':
			stack = append(stack, false)
		case '}':
			if len(stack) == 0 {
				continue
			}
			if stack[len(stack)-1] {
				return true
			}
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			// len == 1 with an unmarked top: absorb the } as literal text;
			// the sole open frame keeps seeking its real close.
		case ',':
			if len(stack) > 0 {
				stack[len(stack)-1] = true
			}
		case '.':
			if len(stack) > 0 && j+1 < len(unquoted) && unquoted[j+1] == '.' {
				stack[len(stack)-1] = true
				j++
			}
		}
	}
	return false
}
