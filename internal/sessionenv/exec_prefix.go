package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

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

// CommandEndsOptionsTerminator reports whether command is a single simple call
// (redirections ignored) that contains a literal `--` end-of-options terminator
// after an optional `exec` prefix.
//
// injectSystemPrompt appends agent-specific flags to the END of the resolved
// program string — claude gains `--plugin-dir`, aider gains `--read`. The first
// `--` after the program makes every word after it a positional argument rather
// than a flag, so a `--` ANYWHERE in the command (not only the last word)
// demotes the appended flag: `claude -- --resume` appends `--plugin-dir` after
// `--resume`, but the `--` already made `--resume` positional, and the appended
// flag is positional too, so claude starts without the af plugin and the
// `/af-*` slash commands are unavailable with no error or diagnostic. The scoped
// account boundary refuses this shape (the leftover `--` is reported as
// undeclared), but an UNSCOPED session skips that boundary, so the
// mis-positioned flag reaches `/bin/sh -c` unchanged — the config-load warning
// is what makes this surface loud. Exported for the config loaders on the same
// precedent as CommandUsesExecSeparator.
//
// A leading `exec --` is consumed by stripExecPrefix first, so `exec -- claude`
// (which CommandUsesExecSeparator already warns about) does not false-positive
// here; `exec -- claude --` correctly does, because the trailing `--` is a
// separate terminator behind the exec separator. Redirections are ignored
// because they do not affect whether a `--` appears among the words.
func CommandEndsOptionsTerminator(command string) bool {
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		return false
	}
	words, _ := stripExecPrefix(call.Args)
	for _, w := range words {
		if lit, ok := literalShellWord(w); ok && lit == "--" {
			return true
		}
	}
	return false
}

// CommandHasControlOperator reports whether command parses as valid shell but is
// NOT a single simple call — i.e. it contains a shell control operator (|, &&,
// ||, ;, &) or a compound construct (subshell, if, for, …) that would route
// af's appended agent-specific flag to the wrong command rather than to the
// agent. `claude | tee /tmp/log` becomes `claude | tee /tmp/log --plugin-dir '…'`
// and the flag reaches tee, not claude.
//
// A single simple call that is TERMINATED by a statement separator is also a
// mismap: `claude;` is one CallExpr (the `;` is a terminator, not a separator
// that splits it into two statements), so it reaches the `return false` below
// and looks like a plain call. But injectSystemPrompt appends at the end, so
// `claude;` becomes `claude; --plugin-dir …` and the flag runs as a separate
// command. The `;` is exposed on stmt.Semicolon; a trailing newline is the other
// statement terminator the parser folds into a single Stmt without a Semicolon,
// so it is checked on the raw value (`claude\n` appends as `claude\n --plugin-dir`
// and the newline starts a new statement just as a `;` would). A `&`/`|&` is
// already caught by stmt.Background/Coprocess above.
//
// Redirections (>, <) do NOT misroute the flag — the agent still receives an
// appended flag regardless of where a redirect appears in a simple call — so a
// single call with redirects is not flagged here. Negation (`! cmd`) is not a
// control operator in the shell sense and does not misroute either, so it is not
// flagged. A parse error is not flagged: such a command fails loudly at launch
// for a different reason. Exported for the config loaders.
func CommandHasControlOperator(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	if len(file.Stmts) == 0 {
		return false // empty / whitespace-only: no command, no control operator
	}
	if len(file.Stmts) != 1 {
		return true // ;, &, and newline-separated multi-statement commands
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Background || stmt.Coprocess || stmt.Disown {
		return true // trailing & background
	}
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return true // |, &&, || (BinaryCmd), (subshell), if/for/while, …
	}
	// A single simple call terminated by `;` (or a trailing newline, which the
	// parser folds into one Stmt without setting Semicolon) still misroutes an
	// appended flag: `claude;` becomes `claude; --plugin-dir …` and the flag runs
	// as its own command. A `&`/`|&` terminator already returned true above.
	if stmt.Semicolon.IsValid() {
		return true
	}
	return hasTrailingNewline(command)
}

// hasTrailingNewline reports whether command ends in a newline that the shell
// parser folds into a single Stmt (it does not set Semicolon for a newline, so
// CommandHasControlOperator has to inspect the raw value). Such a newline
// terminates the command, so appending a flag after it starts a new statement:
// `claude\n` becomes `claude\n --plugin-dir` and the flag runs on its own.
// Trailing spaces/tabs are trimmed first; a newline inside a quoted word is not
// at the end of the value, so it does not trigger this (the closing quote is).
func hasTrailingNewline(command string) bool {
	return strings.HasSuffix(strings.TrimRight(command, " \t"), "\n")
}
