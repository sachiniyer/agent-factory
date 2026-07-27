// Package shellquote renders values as POSIX shell words for the command
// strings af builds and then EXECUTES — the tmux shell-command sessionenv
// wraps, the program string a resumed pane re-runs.
//
// It exists because one nine-line quoter had been copy-pasted into two
// packages: internal/sessionenv.shellQuote and session/tmux.shellQuoteArg were
// byte-identical. That is how a quoting fix lands at one call site and silently
// misses the other — the failure mode #1978 already hit once, when the class was
// understood and fixed at a single site while nine others kept interpolating raw
// values. One home means one place to fix.
//
// Sibling, not duplicate: internal/shellsuggest quotes the commands af PRINTS
// for a human to paste. Its Arg additionally guards zsh start-of-word expansions
// because a pasted command lands in the reader's interactive shell, and that
// guard changes the rendered string — so the two stay separate deliberately, not
// by neglect. The other in-tree quoters (session.shellQuote,
// config.ShellQuotePath) quote unconditionally and so also render differently
// for the same input. Consolidating all of them is a behavior-visible change
// tracked on the structural audit (#1195); this package is the home it lands in.
package shellquote

import "strings"

// shellSafeRunes are the punctuation runes POSIX sh treats literally in an
// unquoted word, so a value built only from these plus alphanumerics needs no
// quoting.
const shellSafeRunes = "_@%+=:,./-"

// Quote renders arg as exactly one POSIX shell word.
//
// A value made only of alphanumerics and shellSafeRunes passes through bare, so
// the common case stays readable in a program string a user may see or edit.
// Anything else is single-quoted, with each embedded single quote escaped as
// '"'"' (close the quote, emit an escaped one, reopen). That makes every other
// metacharacter — space, ", $, backtick, ;, newline — literal. The empty string
// takes that branch too, so it renders as an explicit empty word rather than
// vanishing from the command.
func Quote(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune(shellSafeRunes, r))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}
