package sessionenv

import "mvdan.cc/sh/v3/syntax"

// stripExecPrefix removes a leading `exec` builtin from a command's words and
// reports whether an `exec --` separator was present.
//
// ONE tokenizer, because this package reads the same prefix for two different
// questions and they must not answer it differently. Detection asks WHICH agent
// a command is about; the account boundary asks whether af can prove what the
// pane will run. Both used to carry their own copy of "drop exec, then drop an
// optional --", and #3545 landed a fix on one side of exactly that duplication.
//
// The separator is reported rather than swallowed because `--` is not portable
// across the shells af actually launches into. The pane runs its program through
// `/bin/sh -c`, and POSIX gives the exec builtin no options at all: dash — which
// is /bin/sh on Debian and Ubuntu — takes `--` as the command NAME and exits 127
// with `exec: --: not found`. bash (/bin/sh on macOS, including under --posix),
// busybox ash and zsh in sh mode all accept it. Measured on all five (#3557).
//
// So the callers differ, deliberately:
//
//   - Detection ignores the separator. `exec -- claude` is still a command ABOUT
//     claude, and reporting "unrecognized" there would replace the account
//     boundary's specific refusal with a misleading agent-drift error.
//   - The account boundary refuses it. Which /bin/sh runs the program is not
//     knowable where the command is validated — it is dash on the host here, bash
//     on a macOS host, and busybox ash inside a container or over ssh, where the
//     shell is on a different machine entirely. A validator that predicted an
//     answer would be wrong on some supported platform, so the ambiguous form is
//     refused on all of them.
func stripExecPrefix(words []*syntax.Word) (rest []*syntax.Word, separator bool) {
	if len(words) == 0 || !wordEquals(words[0], "exec") {
		return words, false
	}
	words = words[1:]
	if len(words) > 0 && wordEquals(words[0], "--") {
		return words[1:], true
	}
	return words, false
}

// CommandUsesExecSeparator reports whether a command's exec builtin is followed
// by the `--` separator stripExecPrefix refuses to launch behind.
//
// It re-parses instead of threading a flag out of the guard: the guard answers
// provable/unprovable for a dozen reasons, and this asks the one question the
// refusal message needs to name.
//
// Exported for the config loaders (#3566), which warn about the same shape in an
// operator-authored value that never reaches the account boundary — an unscoped
// session's program, a post-worktree hook, an archive hook. Warning and refusal
// must not answer the question differently, so they share this one predicate
// rather than each carrying a copy of "drop exec, then look for --".
//
// Redirections are ignored rather than disqualifying, which is why this does not
// use singleSimpleCall: `exec -- claude >agent.log` is a shape an operator
// actually writes, and dash fails it for the separator regardless of where its
// output goes. The account boundary refuses such a command either way — it is
// unprovable for the redirection — so widening here only lets it reach for the
// specific message instead of the generic one.
func CommandUsesExecSeparator(command string) bool {
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		return false
	}
	_, separator := stripExecPrefix(call.Args)
	return separator
}
