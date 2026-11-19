package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file models #7603's named launchers — the reset-class programs,
// command-string runners and environment writers the gate knows by name.
// Anything still unmodelled is judged exactly as master and named in the
// gate's stated limit (docs/usage-limits.md) — there is no generic scan
// over arbitrary commands.
//
// The rule (the design decision after six Codex rounds — the per-launcher
// option grammars and every-reading payloads are deleted) is closure by
// construction: the verdict never asks WHERE a launcher's command starts,
// so no finding of the form "option X's position, arity or value hides a
// command" can exist. For the tail of every launcher this file models,
// refuse when ANY of these holds:
//
//   - a literal piece names the account's agent program or a protected
//     variable at identifier boundaries — a piece is every argv word
//     and every piece of a word split on whitespace, `=`, `;`, `&`,
//     `|`, quotes and parentheses, and an agent name also matches as a
//     path basename (`'/opt/agent tools/codex'`, `co""dex`/`co\dex`
//     once the shell's quoting and escapes are normalized);
//   - a word cannot be read (no literal — a substitution or expansion)
//     or a literal carries command-substitution or parameter-expansion
//     text ($ or `) — a name the text cannot see could be constructed
//     at run time;
//   - a word spells input the gate cannot see — the per-launcher
//     hidden-input word table: a descriptor, stdin, or a language it
//     does not parse — the same refusal class `sh -s` gets. Commands
//     and config read from a FILE are the stated limit in
//     docs/usage-limits.md (`ssh -F`, `tmux -f`, a pwsh script path
//     admit the way `bash ./build.sh` does) — the gate judges the
//     visible command line.
//
// Shells keep #7610's exact `-c` judge — this rule only runs on the
// launchers named in accountNamedLauncherNames. Work is a single linear
// pass: each word literalizes once, each piece checks once, and a buried
// launcher reuses the shared reach budget's per-word verdicts.
//
// The accepted cost: a tail that merely MENTIONS a denied name refuses —
// `ssh host 'cat notes/codex.md'` or `tmux new-window 'vim codex.md'`
// name codex at a boundary inside an account tab, and a `$HOME` in a
// watch command is expansion text the gate cannot resolve. That
// over-refusal is measured against the corpus and stated in
// docs/usage-limits.md rather than predicted away.

// namedLauncherPieceBreaks are the delimiters a tail word is split on —
// the boundaries a launcher's own parsing can place a command or name
// at: whitespace, `=`, `;`, `&`, `|`, quotes and parentheses.
func namedLauncherPieceBreak(c rune) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '=', ';', '&', '|', '\'', '"', '`', '(', ')':
		return true
	}
	return false
}

// namedLauncherPieceRefuses applies the boundary rule to one piece: a
// protected variable named at identifier boundaries, or the account's
// own agent program — boundary text or path basename — with an agent
// whose config pin survives through `preserved` skipped (a still-pinned
// agent is not a re-root).
func namedLauncherPieceRefuses(piece string, names, preserved map[string]struct{}) bool {
	if piece == "" {
		return false
	}
	for name := range names {
		if textNamesIdentifierBoundary(piece, name) {
			return true
		}
	}
	for agent, configVar := range accountConfigVars {
		if _, pinned := names[configVar]; !pinned {
			continue
		}
		if _, kept := preserved[configVar]; kept {
			continue
		}
		if textNamesIdentifierBoundary(piece, agent) || filepath.Base(piece) == agent {
			return true
		}
	}
	return false
}

// namedWordKey is one word's memoized verdict slot — keyed on the word
// node, the launcher whose rule applies (hidden-input tables and the
// operand decode differ per name), and whether it ends the tail (a
// payload flag's stdin reading only exists at the end).
type namedWordKey struct {
	word     *syntax.Word
	launcher string
	last     bool
}

// namedWordVerdict is one word's classification under the closure rule:
// refuse is the word's own verdict; payload marks a command-string
// option spelling — one left at the tail's end reads its command from
// stdin.
type namedWordVerdict struct {
	refuse, payload bool
}

// namedLauncherTailRefuses applies the closure rule to a named
// launcher's tail words — one pass, each word judged once by
// namedLauncherWordVerdict. Verdicts are memoized per word on the shared
// reach budget when preserved is empty: a buried launcher's suffix
// judgements rescan the same tail words (ssh … ssh make), and without
// the memo that is a literalize per position per word — quadratic.
// Spending the shared budget down charges each call's scan size, so a
// pathological nesting still fails closed at the bound.
func namedLauncherTailRefuses(words []*syntax.Word, names, preserved map[string]struct{}, launcher string, b *reachBudget) bool {
	if b == nil {
		b = &reachBudget{left: reachWorkLimit, seen: map[string]bool{}, namedWords: map[namedWordKey]namedWordVerdict{}}
	}
	if !b.spend(len(words)) {
		return true
	}
	_, decode := namedLauncherDecodesOperands[launcher]
	memoize := len(preserved) == 0 && b.namedWords != nil
	last := len(words) - 1
	for i, word := range words {
		key := namedWordKey{word: word, launcher: launcher, last: i == last}
		var verdict namedWordVerdict
		seen := false
		if memoize {
			verdict, seen = b.namedWords[key]
		}
		if !seen {
			verdict = namedLauncherWordVerdict(word, i == last, decode, launcher, names, preserved)
			if memoize {
				b.namedWords[key] = verdict
			}
		}
		if verdict.refuse {
			return true
		}
	}
	return false
}

// namedLauncherWordVerdict classifies ONE tail word: no literal →
// refuse; a hidden-input spelling → refuse; `$` or backtick text →
// refuse (a name the text cannot see could be constructed); every piece
// — the literal and its shell-normalized form, each split on the
// delimiter set — checked for the agent and protected names; and a
// payload flag at the tail's end reads stdin.
func namedLauncherWordVerdict(word *syntax.Word, last bool, decode bool, launcher string, names, preserved map[string]struct{}) namedWordVerdict {
	literal, ok := launcherLiteralWord(word)
	if !ok {
		return namedWordVerdict{refuse: true}
	}
	if decode {
		decoded, decOK := gvariantDecode(literal)
		if !decOK {
			return namedWordVerdict{refuse: true}
		}
		literal = decoded
	}
	verdict := namedWordVerdict{
		refuse:  literalNamesHiddenInput(literal, launcher),
		payload: literalSpellsTailPayload(literal, launcher),
	}
	if verdict.refuse {
		return verdict
	}
	if strings.ContainsAny(literal, "$`") {
		return namedWordVerdict{refuse: true}
	}
	// The literal and its normalized form are both judged: a
	// downstream shell removes quoting at every depth — `co""dex`
	// and `co\\\\dex` are both `codex` — so the piece rule sees
	// both the text and the flat-normalized over-approximation of
	// what the text becomes.
	if namedLauncherTextRefuses(literal, names, preserved) {
		return namedWordVerdict{refuse: true}
	}
	if normalized := normalizeNamedLiteral(literal); normalized != literal &&
		namedLauncherTextRefuses(normalized, names, preserved) {
		return namedWordVerdict{refuse: true}
	}
	if last && verdict.payload {
		// A payload flag with no operand reads its command from
		// stdin — the same hidden input.
		return namedWordVerdict{refuse: true}
	}
	return verdict
}

// normalizeNamedLiteral removes the quoting a downstream shell strips
// at ANY depth, in one flat pass: every run of backslashes goes, a line
// break (LF or CRLF) right after a run goes with it — sh's line
// continuation — and every quote character goes. The result is an
// over-approximation of POSIX escape resolution (XCU 2.2.1), closed
// under nesting: quoting quoted again reduces to the same text with no
// recursion and no fixpoint loop. Exact one-level resolution is
// depth-sensitive — inside double quotes `\\` is a literal backslash a
// deeper re-parse would still consume — so an escape can hide a name at
// a depth the exact rule misses; deleting the escapes themselves makes
// `co\\\\dex`, `co\\dex` and `co\dex` all read `codex`. Whitespace is
// kept, so `echo co dex` still admits.
func normalizeNamedLiteral(text string) string {
	if !strings.ContainsAny(text, "'\"`\\") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			for i < len(text) && text[i] == '\\' {
				i++
			}
			switch {
			case i < len(text) && text[i] == '\n':
				i++
			case i+1 < len(text) && text[i] == '\r' && text[i+1] == '\n':
				i += 2
			}
			i-- // the loop's i++ lands past the run (and any line break)
		case '\'', '"', '`':
			// Quote characters vanish at every depth.
		default:
			out.WriteByte(text[i])
		}
	}
	return out.String()
}

// namedLauncherTextRefuses runs the piece rule over one text — the text
// itself plus every piece split on the delimiter set.
func namedLauncherTextRefuses(text string, names, preserved map[string]struct{}) bool {
	if namedLauncherPieceRefuses(text, names, preserved) {
		return true
	}
	for _, piece := range strings.FieldsFunc(text, namedLauncherPieceBreak) {
		if piece != text && namedLauncherPieceRefuses(piece, names, preserved) {
			return true
		}
	}
	return false
}

// unwrapNamedLauncher judges every #7603-named launcher under the one
// closure rule — the name selects the rule and its hidden-input table;
// option structure is never consulted.
func unwrapNamedLauncher(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, launcher string) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand, memo) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, namedLauncherTailRefuses(words, names, nil, launcher, memo.reach)
}

// textNamesIdentifierBoundary reports whether name appears in text
// bounded on both sides by bytes that are not identifier bytes — the
// boundary `echo CODEX_HOME` satisfies and `CODEX_HOME_DIR` does not.
func textNamesIdentifierBoundary(text, name string) bool {
	for i := 0; i+len(name) <= len(text); i++ {
		if text[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isEnvNameByte(text[i-1]) {
			continue
		}
		if i+len(name) < len(text) && isEnvNameByte(text[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}
