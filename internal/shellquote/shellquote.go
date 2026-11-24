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
// One of those folds has since happened: the UNCONDITIONAL variant. A builder
// that wants every value visibly quoted cannot use Quote — Quote("x") emits
// x — so session.shellQuote and app.shellQuote each carried a verbatim copy of
// one unconditional body (app.shellQuote written after this package already
// existed), and config.ShellQuotePath carried the same body behind an
// empty-string carve-out. Wherever their inputs overlapped the copies agreed,
// so the body now lives here once as QuoteAll and the three sites delegate,
// keeping the names their callers and tests are written against — and
// ShellQuotePath's "" carve-out at its delegate site.
//
// Siblings, not duplicates. The quoters that remain differ on some input —
// which is the whole reason they are not folded in here. Stated precisely,
// because the differences are one line each and invisible to a reader merging
// them by eye:
//
//   - internal/shellsuggest.Arg quotes the commands af PRINTS for a human to
//     paste. It additionally guards zsh start-of-word expansions, because a
//     pasted command lands in the reader's interactive shell: Arg("=x") quotes,
//     Quote("=x") does not.
//   - session.shellSingleQuote (session/ptychannel_tmux.go) quotes
//     unconditionally like QuoteAll but uses the other of the tree's two
//     escape idioms — the one Quote uses, not QuoteAll's. Both hand /bin/sh
//     the same word; only the spelled-out text differs, and the pipe-pane
//     tests pin that text.
//   - config.ShellQuotePath carves out the empty string, returning ""
//     unchanged where QuoteAll("") emits the explicit empty word. The
//     delegate keeps the carve-out because an empty value reaching a shell
//     is exactly where "cosmetic" stops being cosmetic.
//
// Folding any of THOSE in is a behavior-visible change, not a mechanical
// move. It is tracked on the structural audit (#1195); this package is the
// home it lands in when someone takes it deliberately.
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

// QuoteAll renders arg single-quoted UNCONDITIONALLY — every input is wrapped,
// so QuoteAll("x") is 'x' where Quote("x") is x, and the empty string renders
// as an explicit empty word rather than vanishing.
//
// It is the one implementation of what was three verbatim copies:
// session.shellQuote (the ssh/docker/sandbox provisioning scripts),
// app.shellQuote (the tmux run-shell strings) and config.ShellQuotePath (paths
// persisted into program_overrides) all carried this exact body — the
// unconditional variant of the same copy-paste this package exists to end.
// Their names and any per-site carve-out stay at the delegate.
//
// The escape idiom differs from Quote's on purpose:
//
//	'   ->   '\''
//
// where Quote closes the quote and emits a double-quoted one ('"'"'). Both
// hand /bin/sh the same word — the spelled text is all that differs — and the
// idiom above is the spelling the folded call sites' tests and persisted
// values pin, which is what makes the fold byte-identical.
func QuoteAll(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
