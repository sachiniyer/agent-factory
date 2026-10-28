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
// the literal character x — the same decode that makes ShellQuotePath's '\”
// apostrophe idiom round-trip) and then
// refuses any word whose UNQUOTED, UNESCAPED characters the shell could still
// act on: a glob metacharacter, a word-leading tilde, or a brace expansion.
// Quoted or escaped copies of the same characters are literal text and stay
// comparable.
func LiteralCommandArgv(command string) ([]string, bool) {
	// A line continuation is removed before everything except reserved-word
	// recognition, which still sees the joined word — the comparator cannot
	// prove how ti\<newline>me lexes, so any backslash-newline anywhere in the
	// source makes the command unparseable (Codex review rounds 6-7). The
	// parser does not retain the pair inside Lit values, so this must be a
	// source-level check; over-rejecting a quoted or escaped copy is safe —
	// the caller falls back to the byte compare.
	if strings.Contains(command, "\\\n") {
		return nil, false
	}
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
// x, \<newline> is unparseable) and simultaneously checked for syntax the
// shell could still act on — an UNESCAPED glob (* ? [), a word-leading tilde
// (the only expansion position the /bin/sh launch shell honors in argument
// position; a ~ after = or : is literal there — Codex review round 7), or an
// unescaped $ immediately before a quote part (the POSIX spelling of
// $'…'/$"…", literal to a POSIX sh but decoded input to bash). unquoted
// accumulates only unescaped Lit characters for the brace check, since every
// separator of a brace expansion must be unquoted and unescaped.
func literalArgvWord(word *syntax.Word) (string, bool, bool) {
	if word == nil {
		return "", false, false
	}
	var value, unquoted strings.Builder
	// wordStart is true until the first byte is emitted or the first quote
	// part is seen — even an empty one — since a ~ only expands at the very
	// front of a word.
	wordStart := true
	bare := true
	// lastUnq/lastUnqInert track the last emitted byte and whether it came
	// from unquoted, unescaped text — the only context whose trailing $ can
	// form the ambiguous $'…'/$"…" spelling with a following quote part. They
	// are maintained incrementally; rescanning the accumulated value is what
	// made earlier versions quadratic (Codex review round 7).
	var lastUnq byte
	lastUnqInert := false
	for i, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.DblQuoted:
			wordStart, bare = false, false
			before := value.Len()
			if !appendLiteralDblQuoted(&value, p, &lastUnq) {
				return "", false, false
			}
			if value.Len() > before {
				lastUnqInert = false
			}
		case *syntax.SglQuoted:
			wordStart, bare = false, false
			if p.Dollar {
				return "", false, false
			}
			for k := 0; k < len(p.Value); k++ {
				value.WriteByte(p.Value[k])
			}
			if len(p.Value) > 0 {
				lastUnq, lastUnqInert = p.Value[len(p.Value)-1], false
			}
		case *syntax.Lit:
			raw := p.Value
			for j := 0; j < len(raw); j++ {
				c := raw[j]
				if c == '\\' {
					bare = false
					if j+1 == len(raw) {
						// Unreachable from a successful parse (a trailing escape
						// continues into the next part); keep it as literal text.
						value.WriteByte('\\')
						lastUnq, lastUnqInert, wordStart = '\\', false, false
						continue
					}
					n := raw[j+1]
					if n == '\n' {
						// Defense in depth: the command-level scan rejects
						// continuations before parsing; keep the refusal here
						// in case a Lit value ever retains the pair.
						return "", false, false
					}
					value.WriteByte(n)
					lastUnq, lastUnqInert, wordStart = n, false, false
					j++
					continue
				}
				switch c {
				case '*', '?':
					return "", false, false
				case '[':
					if bracketMayBeClass(raw, j, i+1 < len(word.Parts)) {
						return "", false, false
					}
				case '~':
					if wordStart {
						return "", false, false
					}
				}
				value.WriteByte(c)
				unquoted.WriteByte(c)
				lastUnq, lastUnqInert, wordStart = c, true, false
			}
			// The $ check runs after the Lit is decoded so an escaped \$ —
			// literal text — is not mistaken for the $'…'/$"…" spelling.
			if lastUnqInert && lastUnq == '$' && i+1 < len(word.Parts) && isQuotedWordPart(word.Parts[i+1]) {
				return "", false, false
			}
		default:
			// ParamExp, CmdSubst, ArithmExp and friends — never literal.
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
// last receives the most recently emitted byte, so the caller's trailing-$
// check needs no rescan of the accumulated buffer.
func appendLiteralDblQuoted(b *strings.Builder, dq *syntax.DblQuoted, last *byte) bool {
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
					*last = raw[j+1]
					j++
					continue
				}
			}
			b.WriteByte(c)
			*last = c
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

// bracketMayBeClass reports whether the unquoted `[` at raw[at] can open a glob
// bracket expression. The shell treats `[` as a pattern only when a closing `]`
// follows at least one member, so `[`, `[[` (the test builtins as plain words),
// and the empty `[]` stay literal — which the xargs -I[] marker and a POSIX
// parse of `[[ … ]]` rely on. A closer that could sit in a later part of the
// word is unknowable here, so it counts as a class (fail closed).
func bracketMayBeClass(raw string, at int, laterParts bool) bool {
	if start := at + 2; start <= len(raw) && strings.IndexByte(raw[start:], ']') >= 0 {
		return true
	}
	return laterParts
}
