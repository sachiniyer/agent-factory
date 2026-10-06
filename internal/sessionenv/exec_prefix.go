package sessionenv

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
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
// separate terminator behind the exec separator. An `env` wrapper's `--` is its
// OWN terminator, not the command's: `env -- claude` passes a trailing
// `--plugin-dir` to claude normally (env's `--` ended env's options), so a `--`
// that belongs to a leading `env` is skipped before scanning for the command's own
// terminator — `env -- claude --` still flags the trailing `--` as claude's
// (#5167 review: "Distinguish wrapper option terminators from agent terminators").
// Redirections are ignored because they do not affect whether a `--` appears
// among the words.
func CommandEndsOptionsTerminator(command string) bool {
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		return false
	}
	words, _ := stripExecPrefix(call.Args)
	words = skipEnvWrapperTerminator(words)
	for _, w := range words {
		if lit, ok := literalShellWord(w); ok && lit == "--" {
			return true
		}
	}
	return false
}

// skipEnvWrapperTerminator drops a leading `env` wrapper's own `--` (and its
// options) so the terminator scan only sees the `--` that belongs to the command
// env runs, not to env. `env -- claude` is a valid GNU env form (the `--` ends
// env's options and the rest is the command), so the `--` is not the command's
// end-of-options marker; `env -- claude --` still has claude's own `--`, which the
// scan then flags. An `env` invocation envcommand.Parse cannot model (an
// unsupported option, or a split-string) is left untouched so the scan stays on
// the safe side of its prior behavior.
//
// env can sit behind another argv-passthrough wrapper: `ionice -c 3 env -- claude`
// is one CallExpr whose first word is `ionice`, so a words[0]-only check left
// env's `--` in the scan and warned even though env passes the appended flag to
// claude normally. The known wrapper prefixes (ionice, nice, nohup, timeout,
// setsid, stdbuf, taskset, command, builtin, …) are peeled first with the same
// unwrapAccountCommand the account walk uses; with no denied names its peel is
// purely structural, and an unsafe peel (a dynamic or unprovable form) falls back
// to the unpeeled words so the scan stays on the safe side of its prior behavior
// (#5167 review: "Recognize env terminators behind wrappers").
func skipEnvWrapperTerminator(words []*syntax.Word) []*syntax.Word {
	if len(words) == 0 {
		return words
	}
	if peeled, unsafe := unwrapAccountCommand(words, map[string]struct{}{}, newOperandTailMemo()); !unsafe {
		words = peeled
	}
	if len(words) == 0 || !wordBaseEquals(words[0], "env") {
		return words
	}
	args, ok := literalCommandArgs(words[1:])
	if !ok {
		return words
	}
	invocation, err := envcommand.Parse(args, envcommand.Policy{AllowAssignments: true})
	if err != nil || invocation.CommandIndex < 0 || 1+invocation.CommandIndex >= len(words) {
		return words
	}
	return words[1+invocation.CommandIndex:]
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
// flagged. A compound command (|, &&, ||) appends the flag to the END of the value,
// which is the rightmost command of the chain. When that rightmost command is the
// detected agent, the flag reaches the agent and the compound is not a misroute:
// `true && claude` becomes `true && claude --plugin-dir …` and claude runs with
// the flag, so it is not flagged; `claude | tee` and `claude && true` still route
// the flag to a command that is not the agent and are flagged. The agent is the
// first agent token DetectAgentFromCommand finds in the value (the same one the
// config loader passes here), and the rightmost command is what receives the
// appended flag, so the warning is restricted to compounds whose rightmost command
// is not the agent (#5167 review: "Do not flag compound commands whose final
// command is the agent"). A non-agent caller passes an empty agent to keep the
// prior behavior of flagging every compound.
//
// A parse error is not flagged unless the value ends with a trailing
// operator that appending a word completes: `claude |` is a parse error on its
// own, but injectSystemPrompt appends `--plugin-dir` to the END of the value, so
// `claude | --plugin-dir …` is valid shell and the flag runs as the right side of
// the pipe rather than as an argument to claude (#5167 review: "Warn when appending
// repairs an incomplete shell operator"). A trailing `|`, `&&`, `||`, or an
// incomplete redirection (`>`, `<`, `>>`) is such a case; a parse error that
// appending does not complete is still not flagged, because the value fails loudly
// at launch for a different reason. Exported for the config loaders.
func CommandHasControlOperator(command, agent string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		// A trailing operator that appending a word completes is a misroute: the
		// injected flag supplies the missing right side of a pipe, the target of a
		// redirection, or the second command of an &&/||, so the flag runs as that
		// rather than as an argument to the agent. A parse error that appending
		// cannot complete is not a misroute and still fails loudly at launch.
		return endsWithIncompleteOperator(command)
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
	if bin, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		// A compound (|, &&, ||) appends to the rightmost command. When that
		// command is the detected agent the flag reaches it, so the compound is
		// not a misroute; otherwise the flag is routed to a non-agent command and
		// the compound is flagged. A non-agent caller (agent == "") keeps the
		// prior behavior of flagging every compound.
		if agent == "" || binaryTailAgentName(bin) != agent {
			return true
		}
		return false
	}
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return true // (subshell), if/for/while, …
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

// binaryTailAgentName returns the base name of the first word of the rightmost
// command in a binary chain (|, &&, ||), after an exec prefix and the known
// argv-passthrough wrappers, or "" when the rightmost command is not a simple
// call. injectSystemPrompt appends to the END of the value, so the flag reaches
// the rightmost command; CommandHasControlOperator uses this to skip the warning
// when that command is the detected agent. The wrapper peel is the same
// skipEnvWrapperTerminator uses (exec, env, ionice/nice/…), so a rightmost
// command reached through a wrapper (`true && exec claude`, `true && env claude`)
// is named the same way DetectAgentFromCommand would name it.
func binaryTailAgentName(bin *syntax.BinaryCmd) string {
	if bin.Y == nil {
		return ""
	}
	cmd := bin.Y.Cmd
	for {
		next, ok := cmd.(*syntax.BinaryCmd)
		if !ok {
			break
		}
		cmd = next.Y.Cmd
	}
	call, ok := cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return ""
	}
	words, _ := stripExecPrefix(call.Args)
	words = skipEnvWrapperTerminator(words)
	if len(words) == 0 {
		return ""
	}
	lit, ok := literalShellWord(words[0])
	if !ok {
		return ""
	}
	return strings.ToLower(filepath.Base(lit))
}

// stripTrailingLineContinuation removes a trailing backslash-newline (a shell
// line continuation) from command so a suffix check sees the operator the
// continuation was hiding. `claude |\<newline>` joins to `claude |` in the shell,
// so the incomplete `|` is what the value ends in once the continuation is gone;
// without the strip a bare suffix check sees the newline, not the `|`, and misses
// the misroute (injectSystemPrompt appends `--plugin-dir` to the joined value,
// completing the pipe and routing the flag to its empty right side). An even run
// of backslashes escapes the newline's escape (the last backslash escapes the
// second-to-last), leaving a real terminator that is NOT a continuation, so it is
// left in place. Whitespace after the newline is trimmed first so a `\<newline>`
// at the very end is found (#5167 review: "Handle incomplete operators followed
// by a continued newline").
func stripTrailingLineContinuation(command string) string {
	trimmed := strings.TrimRight(command, " \t")
	if !strings.HasSuffix(trimmed, "\n") {
		return command
	}
	body := trimmed[:len(trimmed)-1]
	backs := 0
	for len(body) > 0 && body[len(body)-1] == '\\' {
		backs++
		body = body[:len(body)-1]
	}
	if backs%2 == 0 {
		return command
	}
	return strings.TrimRight(body, " \t")
}

// endsWithIncompleteOperator reports whether command ends with a shell operator
// (`|`, `&&`, `||`, `>`, `<`, `>>`) that makes it a parse error on its own but
// that appending a word completes — so the flag injectSystemPrompt appends to the
// END of the value supplies the missing right side of a pipe (`|`/`||`), the
// second command of an `&&`, or the target of a redirection (`>`, `<`, `>>`),
// and the flag runs as that rather than as an argument to the agent. A trailing
// `&` is a valid background operator (handled by stmt.Background above), not a
// parse error, so it is not in this set; a quoted operator (`claude "a|"`) is not
// a parse error and parses as a single call with a literal word.
func endsWithIncompleteOperator(command string) bool {
	command = stripTrailingLineContinuation(command)
	trimmed := strings.TrimRight(command, " \t")
	if trimmed == "" {
		return false
	}
	switch {
	case strings.HasSuffix(trimmed, "|"):
		// `|` is an incomplete pipe and `||` an incomplete or: appending supplies
		// the right side / the second command.
		return true
	case strings.HasSuffix(trimmed, "&&"):
		// `&&` is an incomplete and: appending supplies the second command.
		return true
	case strings.HasSuffix(trimmed, ">"), strings.HasSuffix(trimmed, "<"):
		// A bare `>`/`<`/`>>` is an incomplete redirection: appending supplies the
		// target, so the flag becomes the redirect target rather than a flag.
		return true
	}
	return false
}

// CommandHasTrailingComment reports whether command is a single simple call
// (redirections ignored) whose last word is followed by a `#` shell comment that
// is the final thing on its line. injectSystemPrompt appends its agent-specific
// flag to the END of the resolved program string, and everything after a `#` on
// the same line is a comment, so the appended flag lands inside the comment and
// is discarded: `claude # use the default profile` becomes
// `claude # use the default profile --plugin-dir '…'`, and claude starts without
// the af plugin. A `#` glued to a word (`claude#foo`) is not a comment, and a `#`
// inside quotes (`claude 'a#b'`) is not a comment; the parser's comment handling
// reflects both, so the detection is on the parsed comment, not the raw `#`.
//
// A comment that is NOT the last thing on the line does not swallow the flag: a
// trailing newline starts a new statement that carries the flag, which the
// control-operator predicate already warns about, so this only fires when the
// comment is the final content on the last line (hasTrailingNewline is false). A
// leading comment (`# comment\nclaude`) is before the command, so the flag —
// appended after the command — is not in it.
//
// Exported for the config loaders on the same precedent as
// CommandUsesExecSeparator: the same value reaches injectSystemPrompt's
// flag-appending branch, and the comment swallows the appended flag there too.
func CommandHasTrailingComment(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX), syntax.KeepComments(true)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Background || stmt.Coprocess || stmt.Disown {
		return false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	if len(stmt.Comments) == 0 {
		return false
	}
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 {
		return false
	}
	lastEnd := words[len(words)-1].End().Offset()
	// A trailing newline after the comment starts a new statement that carries
	// the flag, which CommandHasControlOperator already warns about; the comment
	// only swallows the flag when it is the final content on the last line.
	if hasTrailingNewline(command) {
		return false
	}
	for _, cm := range stmt.Comments {
		if cm.Hash.Offset() > lastEnd {
			return true
		}
	}
	return false
}

// CommandInvokesAgentViaInterpreter reports whether command runs the agent through
// a shell interpreter's `-c` flag, so the agent is the script and the flag
// injectSystemPrompt appends to the END of the value lands after the script as a
// positional to the interpreter, not as an argument to the agent. `sh -c 'claude'`
// is a single CallExpr, so the terminator/control-operator/comment predicates do
// not flag it; DetectAgentFromCommand still finds `claude` in the quoted script
// token, so the flag is appended (`sh -c 'claude' --plugin-dir '…'`) and the
// plugin never reaches claude (#5167 review: "Warn when an agent is invoked through
// `sh -c`"). A plain `sh claude` (no `-c`) runs `claude` as a script FILE, which is
// a different shape the account boundary already refuses, so it is not flagged here.
//
// Exported for the config loaders on the same precedent as CommandUsesExecSeparator.
func CommandInvokesAgentViaInterpreter(command string) bool {
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		return false
	}
	words, _ := stripExecPrefix(call.Args)
	// An `env` (or argv-passthrough wrapper) prefix can sit in front of the
	// interpreter: `env sh -c 'claude'` reaches Claude detection because
	// DetectAgentFromCommand scans through `env` to the interpreter, but a
	// words[0]-only check here saw `env` and returned false, so the misroute went
	// unwarned. Peel the same wrapper prefix skipEnvWrapperTerminator uses so the
	// interpreter — not the wrapper — is what the shell check runs on. `env -- sh
	// -c 'claude'` peels env's `--` too, so the interpreter is reached the same way
	// (#5167 review: "Unwrap interpreter wrappers before checking `-c`").
	words = skipEnvWrapperTerminator(words)
	if len(words) < 2 {
		return false
	}
	// The first word (after an optional exec and wrapper) is the interpreter; it
	// must be a known shell for the `-c` script shape to apply. `exec sh -c
	// 'claude'` strips the exec prefix first, leaving `sh -c 'claude'`.
	if !wordIsKnownShell(words[0]) {
		return false
	}
	// The `-c` flag means the next word is a script, so the agent the flag is
	// appended after is the interpreter, not the agent named inside the script.
	// The standalone spelling is a literal `-c`; a combined short-option cluster
	// such as `bash -ic 'claude'` also carries the `-c` flag (bash --help lists
	// `-ilrsD or -c command`), so shellShortClusterHasC recognizes `c` in a
	// cluster as well. After injection `bash -ic 'claude' --plugin-dir …` makes
	// the appended words the shell's `$0`/positionals, so claude starts without
	// the plugin (#5167 review: "Detect combined shell -c options").
	for _, w := range words[1:] {
		lit, ok := literalShellWord(w)
		if !ok {
			continue
		}
		if lit == "-c" || shellShortClusterHasC(lit) {
			return true
		}
	}
	return false
}

// wordIsKnownShell reports whether the first word is a literal invocation of a
// shell interpreter (by basename), the shape whose `-c` runs a script and whose
// appended flag is a positional to the interpreter rather than to the agent
// inside the script. knownShellName is the shared list the account environment
// walk keeps for the same shells.
func wordIsKnownShell(word *syntax.Word) bool {
	value, ok := literalShellWord(word)
	if !ok {
		return false
	}
	return knownShellName(filepath.Base(value))
}

// shellShortClusterHasC reports whether a combined short-option word (a word
// starting with a single `-` and more than one flag character) carries the `-c`
// flag that makes the next word a script, as in `bash -ic 'claude'`. Bash and
// the other POSIX shells accept the compact form (`bash --help` lists `-ilrsD
// or -c command`); the standalone `-c` is already matched by the literal scan
// in CommandInvokesAgentViaInterpreter, so this inspects only a cluster with
// more than one flag. A flag that takes an argument would swallow a later `c`
// as its operand (bash's `-O shopt_option`), so the scan stops at the first flag
// not known to be argument-free for the POSIX shells; `c` itself ends the scan
// because it consumes the next word as its script. The argument-free invocation
// flags are `e`, `i`, `l`, `s`, `r`, and `D` (bash's `-e` errexit and dash's
// `-e` are both argument-free, so `bash -ec 'claude'` is a `-c` form); a `c`
// after any other flag is not claimed as the script flag, so an unknown option
// stays conservative rather than over-warning (#5167 review: "Recognize `-e` in
// shell `-c` option clusters").
func shellShortClusterHasC(word string) bool {
	if len(word) <= 2 || word[0] != '-' || word[1] == '-' {
		return false
	}
	for _, flag := range word[1:] {
		switch flag {
		case 'c':
			return true
		case 'e', 'i', 'l', 's', 'r', 'D':
			continue
		default:
			return false
		}
	}
	return false
}

// CommandHasHeredoc reports whether command is a single simple call with a
// here-document redirect (`<<` or `<<-`) whose body is the last content on the
// last line. injectSystemPrompt appends its flag to the END of the value, so the
// flag lands inside the here-document body (or breaks the closing delimiter, which
// makes the body consume the rest of the line) rather than reaching the agent:
// `claude <<EOF\nprompt\nEOF` becomes `claude <<EOF\nprompt\nEOF --plugin-dir …`,
// and the `EOF --plugin-dir …` is no longer the closing delimiter (it has trailing
// content), so the flag is read as here-document input and claude starts without
// it (#5167 review: "Handle here-document delimiters before appending flags").
// A trailing newline after the closing delimiter starts a new statement that
// carries the flag, which the control-operator predicate already warns about, so
// this only fires when the heredoc is the final content on the last line. An
// ordinary input/output redirection (`>`, `<`) does NOT misroute the flag — the
// agent still receives it — so only the here-document operators are checked.
//
// Exported for the config loaders on the same precedent as CommandUsesExecSeparator.
func CommandHasHeredoc(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return false
	}
	stmt := file.Stmts[0]
	if stmt == nil {
		return false
	}
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return false
	}
	for _, r := range stmt.Redirs {
		if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc {
			// A trailing newline after the closing delimiter starts a new statement
			// that carries the flag, which CommandHasControlOperator already warns
			// about; the heredoc only swallows the flag when its body is the last
			// content on the last line (no trailing newline).
			return !hasTrailingNewline(command)
		}
	}
	return false
}

// hasTrailingNewline reports whether command ends in a newline that the shell
// parser folds into a single Stmt (it does not set Semicolon for a newline, so
// CommandHasControlOperator has to inspect the raw value). Such a newline
// terminates the command, so appending a flag after it starts a new statement:
// `claude\n` becomes `claude\n --plugin-dir` and the flag runs on its own.
// Trailing spaces/tabs are trimmed first; a newline inside a quoted word is not
// at the end of the value, so it does not trigger this (the closing quote is).
//
// A backslash-newline is a shell line continuation, not a statement terminator:
// `claude \\\n` (a trailing backslash followed by a newline) keeps the appended
// flag on the same command, so it is NOT a control operator (#5167 review:
// "Respect escaped trailing newlines"). An even run of backslashes escapes the
// newline's escape (the last backslash escapes the second-to-last, leaving the
// newline unescaped), so only an ODD run immediately before the newline is a
// continuation; an even run (including zero) leaves a real terminator.
func hasTrailingNewline(command string) bool {
	trimmed := strings.TrimRight(command, " \t")
	if !strings.HasSuffix(trimmed, "\n") {
		return false
	}
	body := trimmed[:len(trimmed)-1]
	backs := 0
	for len(body) > 0 && body[len(body)-1] == '\\' {
		backs++
		body = body[:len(body)-1]
	}
	return backs%2 == 0
}
