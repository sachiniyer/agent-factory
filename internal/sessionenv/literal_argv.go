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
// The word walk is deliberately STRICTER than the proof's literalShellWord
// (Codex review on #5487): literalShellWord emits a syntax.Lit part's raw
// bytes, so an unquoted `*` or `~` on disk would be reported as the literal
// word `*` or `~` even though the shell expands it at exec time. This walker
// returns each word's literal text only when every unquoted character is
// provably inert; a word the shell could still rewrite is unanswerable, and
// the caller must fail closed on its own terms.
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
// order is not canonicalized. Quoting a word differently is not a difference;
// an added, removed, reordered, or expandable word is. A command that is not
// a plain literal word list is not equal to anything — including itself — so
// a trust decision fails closed and a drift check falls back to comparing
// bytes.
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

// literalArgvWord renders one word to its literal text under the same quoting
// rules literalShellWord applies, then proves the text means what it says.
// Quoted parts contribute their text verbatim; every character an UNQUOTED
// part contributes is checked for shell syntax the raw bytes hide — an
// unquoted glob or escape rewrites or multiplies the word at exec time, a
// tilde in expansion position resolves a home directory, and an unquoted
// brace expansion splits it into several words. Quoted copies of the same
// characters are literal text and stay comparable.
func literalArgvWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var value, unquoted strings.Builder
	for i, part := range word.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			if !appendLiteralShellPart(&value, part) {
				return "", false
			}
			continue
		}
		// A bare $ immediately before a quote part is the POSIX spelling of
		// $'…' or $"…" — literal text to a POSIX sh but decoded input to bash.
		// The same source produces different words in different shells, so it
		// is not a provable literal.
		if strings.HasSuffix(lit.Value, "$") && i+1 < len(word.Parts) && isQuotedWordPart(word.Parts[i+1]) {
			return "", false
		}
		if literalExpandableChars(lit.Value, &value) {
			return "", false
		}
		value.WriteString(lit.Value)
		unquoted.WriteString(lit.Value)
	}
	if braceExpandable(unquoted.String()) {
		return "", false
	}
	return value.String(), true
}

func isQuotedWordPart(part syntax.WordPart) bool {
	switch part.(type) {
	case *syntax.SglQuoted, *syntax.DblQuoted:
		return true
	}
	return false
}

// literalExpandableChars reports whether the raw text of one UNQUOTED
// syntax.Lit part carries characters the shell would still act on: an escape
// (\ — the runtime word differs from the written bytes), a glob metacharacter
// (* ? [ — the word can expand into zero or more words), or a tilde in
// expansion position (word start, or after = or : in the assignment-form
// positions bash expands even in argument position). assembled is the word
// text built so far; it supplies the preceding character for a part-leading
// tilde.
func literalExpandableChars(litValue string, assembled *strings.Builder) bool {
	for i := 0; i < len(litValue); i++ {
		switch litValue[i] {
		case '\\', '*', '?', '[':
			return true
		case '~':
			switch {
			case i > 0:
				if litValue[i-1] == '=' || litValue[i-1] == ':' {
					return true
				}
			case assembled.Len() == 0:
				return true
			default:
				if prev := assembled.String()[assembled.Len()-1]; prev == '=' || prev == ':' {
					return true
				}
			}
		}
	}
	return false
}

// braceExpandable reports whether the concatenated UNQUOTED text of a word
// carries a brace expansion — {x,y} or {x..y} — which bash (and bash as
// /bin/sh) expands into several words before quote removal. Concatenating
// only unquoted text is what models the rule correctly: every separator must
// be unquoted, so a{','}b renders literal while a{'x',y}b expands. An
// unclosed { or braces without a comma or .. are literal text.
func braceExpandable(unquoted string) bool {
	for i := 0; i < len(unquoted); i++ {
		if unquoted[i] != '{' {
			continue
		}
		for j := i + 1; j < len(unquoted); j++ {
			if unquoted[j] == '}' {
				break
			}
			if unquoted[j] == ',' ||
				(unquoted[j] == '.' && j+1 < len(unquoted) && unquoted[j+1] == '.') {
				if strings.IndexByte(unquoted[j:], '}') >= 0 {
					return true
				}
				break
			}
		}
	}
	return false
}
