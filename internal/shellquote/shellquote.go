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
// Siblings, not duplicates. Three other quoters remain in the tree, and NO TWO
// OF THEM AGREE on some input — which is the whole reason none of them was
// folded in here. Stated precisely, because the differences are one line each
// and invisible to a reader merging them by eye:
//
//   - internal/shellsuggest.Arg quotes the commands af PRINTS for a human to
//     paste. It additionally guards zsh start-of-word expansions, because a
//     pasted command lands in the reader's interactive shell: Arg("=x") quotes,
//     Quote("=x") does not.
//   - session.shellQuote (session/systemprompt.go) quotes UNCONDITIONALLY, for
//     the ssh/docker/sandbox provisioning scripts: shellQuote("x") is 'x' where
//     Quote("x") is x. The empty string becomes a quoted empty word.
//   - config.ShellQuotePath quotes unconditionally TOO, and uses the same escape
//     idiom — but it carves out the empty string and returns it UNCHANGED. So
//     the pair that looks most alike disagrees on "": one emits a quoted empty
//     word, the other emits nothing at all. Folding them would break one caller
//     or the other depending purely on which body survived, and an empty value
//     reaching a shell is exactly where that stops being cosmetic.
//
// Folding any of them in is therefore a behavior-visible change, not a
// mechanical move. It is tracked on the structural audit (#1195); this package
// is the home it lands in when someone takes it deliberately.
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
