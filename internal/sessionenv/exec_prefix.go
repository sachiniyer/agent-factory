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
	return wordsContainOptionsTerminator(words)
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
//
// env can also sit in front of ANOTHER wrapper: `env -- nice -- claude` leaves
// `nice -- claude` after env's own `--` is consumed, and nice's `--` is nice's own
// terminator (GNU nice passes the appended flag through to claude), so the scan
// would otherwise warn on a `--` that does not belong to the agent. The peel and
// the env consumption run in a loop so a wrapper chain (env behind a wrapper, a
// wrapper behind env, nested env) is fully unwrapped before the terminator scan
// sees the words (#5167 review: "Continue unwrapping wrappers after `env`").
func skipEnvWrapperTerminator(words []*syntax.Word) []*syntax.Word {
	for {
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
		words = words[1+invocation.CommandIndex:]
	}
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
// A multi-statement command (`;`- or newline-separated) appends the flag to the
// END of the value, which is the LAST statement, so the same tail-agent analysis
// applies: `true; claude` routes the flag to claude and is not a misroute, while
// `claude; echo hi` routes it to echo and is. The last statement is evaluated
// comprehensively (agent name, `--`, trailing comment, heredoc, terminator,
// trailing backslash) because the single-call predicates
// (CommandEndsOptionsTerminator, CommandHasTrailingComment, CommandHasHeredoc)
// only inspect a one-statement value, so a `--`/comment/heredoc on the last
// statement of a multi-statement value would otherwise go unwarned (#5167 review:
// "Exempt terminal agents in statement sequences"). A non-agent caller passes
// an empty agent to keep the prior behavior of flagging every multi-statement.
//
// A single simple call that ends with an odd run of bare backslashes (and no
// trailing newline) also misroutes the flag: injection concatenates a leading
// space, and the trailing backslash escapes that space, so `claude \` becomes
// `claude \ --plugin-dir …` and the `\ ` joins the injected flag into one literal
// word ` --plugin-dir` (with a leading space) rather than the `--plugin-dir`
// option, so claude starts without the plugin. An even run leaves a literal
// backslash arg and the injected space splits normally, so it is not flagged
// (#5167 review: "Flag a trailing escaped append separator").
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
	// Comments are kept so a trailing `#` on the last command of a compound or a
	// multi-statement value (the single-call CommandHasTrailingComment does not
	// see those) can be detected: the comment swallows the appended flag the same
	// way it does on a single call, and the comment is on the parsed statement
	// only when KeepComments is set.
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX), syntax.KeepComments(true)).Parse(strings.NewReader(command), "")
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
		// A multi-statement command appends to the LAST statement. A non-agent
		// caller (agent == "") keeps the prior behavior of flagging every
		// multi-statement; otherwise the last statement is evaluated for the
		// same tail-agent exemption a binary command gets (#5167 review:
		// "Exempt terminal agents in statement sequences").
		if agent == "" {
			return true
		}
		// The tail-agent exemption holds only when the tail is the ONLY
		// selected-agent invocation. injectSystemPrompt selects the FIRST agent
		// DetectAgentFromCommand finds, so a statement list that invokes the
		// agent earlier AND at the tail leaves the earlier (normally interactive)
		// invocation without the flag: `claude; claude` becomes
		// `claude; claude --plugin-dir …`, the first claude starts without the
		// plugin, and the tail-name match would otherwise hide it. An earlier
		// invocation that selects the agent is itself a misroute, so the statement
		// list warns before the tail exemption applies. The tail is still
		// evaluated after this check because a terminator/comment/`--` on the tail
		// misroutes even when no earlier statement selects the agent (#5167
		// review: "Warn when an earlier statement also invokes the agent").
		for _, s := range file.Stmts[:len(file.Stmts)-1] {
			if stmtInvokesAgent(s, agent) {
				return true
			}
		}
		return stmtMismapsTail(file.Stmts[len(file.Stmts)-1], agent, command)
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Background || stmt.Coprocess || stmt.Disown {
		return true // trailing & background
	}
	if bin, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		return binaryMismaps(bin, stmt, agent, command)
	}
	if _, ok := stmt.Cmd.(*syntax.CallExpr); !ok {
		return true // (subshell), if/for/while, …
	}
	// A single simple call terminated by `;` (or a trailing newline, which the
	// parser folds into one Stmt without setting Semicolon) still misroutes an
	// appended flag: `claude;` becomes `claude; --plugin-dir …` and the flag runs
	// as its own command. A `&`/`|&` terminator already returned true above. A
	// `--`/comment/heredoc on a single simple call is handled by the dedicated
	// predicates (CommandEndsOptionsTerminator, CommandHasTrailingComment,
	// CommandHasHeredoc), so this path does not re-check them; a trailing odd
	// backslash run has no dedicated predicate and is checked here.
	if stmt.Semicolon.IsValid() {
		return true
	}
	if hasTrailingNewline(command) {
		return true
	}
	return endsWithOddBackslashRun(command)
}

// binaryMismaps reports whether a binary command (|, &&, ||) misroutes the
// appended flag given the detected agent. It is the shared evaluation behind the
// single-statement binary branch and the multi-statement last-statement branch
// (when the last statement is itself a binary command). The flag is appended to
// the END of the value, which is the rightmost command of the chain, so the
// compound is a misroute only when that command is not the agent or the flag
// cannot reach it: a `--` inside the tail call, a trailing comment after the
// tail, a heredoc on the tail, a statement terminator (`;`/newline) after the
// compound, or a trailing odd backslash run that escapes the injected separator.
// A non-agent caller (agent == "") keeps the prior behavior of flagging every
// compound.
func binaryMismaps(bin *syntax.BinaryCmd, stmt *syntax.Stmt, agent, command string) bool {
	if agent == "" || binaryTailAgentName(bin) != agent {
		return true
	}
	// The tail-agent exemption holds only when the tail is the ONLY
	// selected-agent invocation. injectSystemPrompt selects the FIRST agent
	// DetectAgentFromCommand finds, so when the agent appears earlier in the
	// compound the selected agent is that earlier invocation, which the flag
	// appended to the END never reaches: `claude && claude` becomes
	// `claude && claude --plugin-dir …`, the first (normally interactive)
	// claude starts without the plugin, and the tail-name match would otherwise
	// hide it. Require the left of the binary to be agent-free before applying
	// the exemption (#5167 review: "Warn when an earlier matching agent misses
	// the appended flag").
	if binaryLeftInvokesAgent(bin, agent) {
		return true
	}
	if binaryTailHasOptionsTerminator(bin) {
		return true
	}
	if binaryTailHasTrailingComment(bin, stmt, command) {
		return true
	}
	if binaryTailHasHeredoc(bin, command) {
		return true
	}
	if stmt.Semicolon.IsValid() {
		return true
	}
	if hasTrailingNewline(command) {
		return true
	}
	return endsWithOddBackslashRun(command)
}

// stmtMismapsTail reports whether the last statement of a multi-statement command
// misroutes the appended flag given the detected agent. The flag is appended to
// the END of the value, which is the last statement, so the analysis is the same
// as a single statement's: a backgrounded/structured (non-call, non-binary)
// statement misroutes; a binary last statement is evaluated by binaryMismaps; a
// simple call is checked comprehensively (agent name, `--`, trailing comment,
// heredoc, terminator, trailing backslash) because the single-call predicates
// only inspect a one-statement value and a `--`/comment/heredoc on the last
// statement of a multi-statement value would otherwise go unwarned.
func stmtMismapsTail(stmt *syntax.Stmt, agent, command string) bool {
	if stmt == nil || stmt.Background || stmt.Coprocess || stmt.Disown {
		return true
	}
	if bin, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		return binaryMismaps(bin, stmt, agent, command)
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return true // (subshell), if/for/while, …
	}
	words, _ := stripExecPrefix(call.Args)
	words = skipEnvWrapperTerminator(words)
	if agent == "" || firstWordAgentName(words) != agent {
		return true
	}
	if wordsContainOptionsTerminator(words) {
		return true
	}
	if stmtTrailingComment(stmt, words, command) {
		return true
	}
	if stmtHasHeredocRedirect(stmt, command) {
		return true
	}
	if stmt.Semicolon.IsValid() {
		return true
	}
	if hasTrailingNewline(command) {
		return true
	}
	return endsWithOddBackslashRun(command)
}

// endsWithOddBackslashRun reports whether command ends with an odd run of bare
// backslashes and no trailing newline, the shape that escapes the leading space
// injectSystemPrompt concatenates onto the value. `claude \` becomes
// `claude \ --plugin-dir …`: the `\ ` escapes the space, joining the injected
// flag into one literal word ` --plugin-dir` (leading space) rather than the
// `--plugin-dir` option, so the agent starts without the plugin. An even run
// leaves a literal backslash arg and the injected space splits the flag into its
// own word, so it is not flagged. A trailing newline is a statement terminator
// (handled by hasTrailingNewline) or a line continuation (an odd run before it),
// neither of which this check is for, so a value that ends with a newline is not
// flagged here. Trailing whitespace after the backslash already separates the
// injected flag, so only a value whose final character is a backslash is checked
// (#5167 review: "Flag a trailing escaped append separator").
func endsWithOddBackslashRun(command string) bool {
	if strings.HasSuffix(command, "\n") {
		return false
	}
	backs := 0
	for i := len(command) - 1; i >= 0 && command[i] == '\\'; i-- {
		backs++
	}
	return backs%2 == 1
}

// firstWordAgentName returns the base name of the first word of a simple call
// (after peeling), lowercased, or "" when the first word is not a literal. It is
// the shared name extraction behind binaryTailAgentName (the rightmost command of
// a binary chain) and the multi-statement tail check (the last statement).
func firstWordAgentName(words []*syntax.Word) string {
	if len(words) == 0 {
		return ""
	}
	lit, ok := literalShellWord(words[0])
	if !ok {
		return ""
	}
	return strings.ToLower(filepath.Base(lit))
}

// wordsContainOptionsTerminator reports whether words hold a literal `--`
// end-of-options terminator anywhere in the list. It is the shared scan behind
// CommandEndsOptionsTerminator (a single simple call) and the binary/multi-statement
// tail checks: the appended flag is positional after a `--`, so a `--` anywhere
// in the command that receives the flag demotes it. The Bash dollar-quoted
// spelling `$'--'` passes a literal `--` to the agent the same way, so it is
// recognized too (#5167 review: "Parse Bash-quoted `--` terminators").
func wordsContainOptionsTerminator(words []*syntax.Word) bool {
	for _, w := range words {
		if wordIsOptionsTerminator(w) {
			return true
		}
	}
	return false
}

// wordIsOptionsTerminator reports whether word is a literal `--` end-of-options
// terminator, including the Bash dollar-quoted spelling `$'--'`. literalShellWord
// treats `$'...'` as non-literal (the `$` is a separate Lit and the quoted body
// may carry C-escapes), so a plain `lit == "--"` misses `$'--'`, which Bash
// passes to the agent as a literal `--` that demotes the appended flag exactly
// like an unquoted `--`. Only a dollar-quoted body with no backslash escapes is
// read as literal — `$'\n'` is a real escape and stays non-literal, the safe
// direction.
func wordIsOptionsTerminator(word *syntax.Word) bool {
	if lit, ok := literalShellWord(word); ok && lit == "--" {
		return true
	}
	if word == nil || len(word.Parts) != 2 {
		return false
	}
	dollar, ok := word.Parts[0].(*syntax.Lit)
	if !ok || dollar.Value != "$" {
		return false
	}
	sq, ok := word.Parts[1].(*syntax.SglQuoted)
	if !ok {
		return false
	}
	return sq.Value == "--" && !strings.Contains(sq.Value, "\\")
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
	return firstWordAgentName(binaryTailCallWords(bin))
}

// binaryLeftInvokesAgent reports whether the left side of a binary command
// (|, &&, ||) — everything except the rightmost command that receives the
// appended flag — invokes the detected agent, including inside a compound
// construct (subshell, if, for, ...) on the left. binaryMismaps uses this to
// keep the tail-agent exemption from applying when the agent appears earlier in
// the compound, where the flag appended to the END never reaches it (#5167
// review: "Warn when an earlier matching agent misses the appended flag"; "Detect
// agent invocations inside earlier compound statements").
func binaryLeftInvokesAgent(bin *syntax.BinaryCmd, agent string) bool {
	return stmtInvokesAgent(bin.X, agent)
}

// stmtInvokesAgent reports whether stmt invokes the detected agent as the first
// word of any simple call within it, including simple calls nested inside a
// compound construct (subshell, brace block, if/while/for/case, ...). It walks a
// binary command's both sides, a simple call directly, and the statements held by
// a compound construct, so an earlier agent invocation that hides inside a
// compound is detected: `(claude) && claude` appends the flag only to the tail
// claude and leaves the subshell's claude without the plugin, so the exemption
// must not treat the compound as agent-free (#5167 review: "Detect agent
// invocations inside earlier compound statements"). `exec` carries its own
// options before the command it runs (`exec -a af claude`), and stripExecPrefix
// consumes only the `exec` keyword, so the exec-option peel is applied before
// the first word is read; without it, `-a` would read as the command and the
// earlier agent call would go undetected (#5167 review: "Peel exec options
// before scanning earlier agent invocations").
func stmtInvokesAgent(stmt *syntax.Stmt, agent string) bool {
	if stmt == nil {
		return false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return stmtInvokesAgent(cmd.X, agent) || stmtInvokesAgent(cmd.Y, agent)
	case *syntax.CallExpr:
		if len(cmd.Args) == 0 {
			return false
		}
		words, _ := stripExecPrefix(cmd.Args)
		words = skipExecOptions(words)
		words = skipEnvWrapperTerminator(words)
		return firstWordAgentName(words) == agent
	case *syntax.Subshell:
		return stmtsInvokeAgent(cmd.Stmts, agent)
	case *syntax.Block:
		return stmtsInvokeAgent(cmd.Stmts, agent)
	case *syntax.IfClause:
		return ifClauseInvokesAgent(cmd, agent)
	case *syntax.WhileClause:
		return stmtsInvokeAgent(cmd.Cond, agent) || stmtsInvokeAgent(cmd.Do, agent)
	case *syntax.ForClause:
		return stmtsInvokeAgent(cmd.Do, agent)
	case *syntax.CaseClause:
		for _, item := range cmd.Items {
			if stmtsInvokeAgent(item.Stmts, agent) {
				return true
			}
		}
		return false
	case *syntax.CoprocClause:
		return stmtInvokesAgent(cmd.Stmt, agent)
	case *syntax.TimeClause:
		return stmtInvokesAgent(cmd.Stmt, agent)
	}
	return false
}

// stmtsInvokeAgent reports whether any statement in stmts invokes the detected
// agent. It is the slice-level scan behind stmtInvokesAgent's compound-construct
// cases.
func stmtsInvokeAgent(stmts []*syntax.Stmt, agent string) bool {
	for _, s := range stmts {
		if stmtInvokesAgent(s, agent) {
			return true
		}
	}
	return false
}

// ifClauseInvokesAgent reports whether an if/elif/else chain invokes the detected
// agent in any of its condition or body statement lists. The Else field chains
// the next elif/else as a nested IfClause, so the whole chain is walked.
func ifClauseInvokesAgent(c *syntax.IfClause, agent string) bool {
	if c == nil {
		return false
	}
	if stmtsInvokeAgent(c.Cond, agent) || stmtsInvokeAgent(c.Then, agent) {
		return true
	}
	return ifClauseInvokesAgent(c.Else, agent)
}

// binaryTailCallWords returns the words of the rightmost simple call in a binary
// chain after an exec prefix and the known argv-passthrough wrappers, or nil
// when the rightmost command is not a simple call. It is the shared walk behind
// binaryTailAgentName (the agent name is the first word) and
// binaryTailHasOptionsTerminator (the terminator scan needs the full tail).
func binaryTailCallWords(bin *syntax.BinaryCmd) []*syntax.Word {
	if bin.Y == nil {
		return nil
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
		return nil
	}
	words, _ := stripExecPrefix(call.Args)
	words = skipEnvWrapperTerminator(words)
	return words
}

// binaryTailHasOptionsTerminator reports whether the rightmost simple call in a
// binary chain contains a literal `--` end-of-options terminator after an
// optional exec prefix and an env wrapper's own terminator. The appended flag
// is positional after a `--`, so a compound whose rightmost command is the agent
// but carries its own `--` still misroutes the flag even though the compound's
// tail is the agent: `true && claude --` becomes
// `true && claude -- --plugin-dir …` and claude starts without the plugin.
// CommandEndsOptionsTerminator only inspects a single simple call, so a binary
// command's tail `--` would otherwise be exempted by the tail-agent check and go
// unwarned (#5167 review: "Inspect terminal `--` inside compound commands").
func binaryTailHasOptionsTerminator(bin *syntax.BinaryCmd) bool {
	return wordsContainOptionsTerminator(binaryTailCallWords(bin))
}

// binaryTailStmt returns the rightmost Stmt of a binary chain (|, &&, ||), the
// statement whose command receives the appended flag and whose redirects/comments
// govern whether the flag actually reaches the agent. A chain is left-
// associative, so the rightmost command is reached by following Y.
func binaryTailStmt(bin *syntax.BinaryCmd) *syntax.Stmt {
	if bin.Y == nil {
		return nil
	}
	stmt := bin.Y
	for {
		next, ok := stmt.Cmd.(*syntax.BinaryCmd)
		if !ok {
			return stmt
		}
		stmt = next.Y
	}
}

// binaryTailHasTrailingComment reports whether a `#` shell comment on the
// enclosing statement follows the rightmost simple call's last word and is the
// final content on the last line. A trailing comment swallows the appended flag
// (`true && claude # note` becomes `true && claude # note --plugin-dir …`, and
// the flag lands inside the comment), so a compound whose tail is the agent still
// misroutes the flag when a comment follows that tail. CommandHasTrailingComment
// only inspects a single simple call, so a binary command's tail comment would
// otherwise be exempted by the tail-agent check and go unwarned (#5167 review:
// "Scan tail comments in terminal compounds").
func binaryTailHasTrailingComment(bin *syntax.BinaryCmd, stmt *syntax.Stmt, command string) bool {
	words := binaryTailCallWords(bin)
	if len(words) == 0 {
		return false
	}
	return stmtTrailingComment(stmt, words, command)
}

// binaryTailHasHeredoc reports whether the rightmost statement of a binary chain
// carries a here-document redirect whose body is the last content on the last
// line. The appended flag lands inside the here-document (or breaks the closing
// delimiter), so a compound whose tail is the agent still misroutes the flag when
// that tail has a heredoc: `true && claude <<EOF\nprompt\nEOF` becomes
// `true && claude <<EOF\nprompt\nEOF --plugin-dir …` and claude starts without
// the plugin. CommandHasHeredoc only inspects a single simple call, so a binary
// command's tail heredoc would otherwise be exempted by the tail-agent check
// and go unwarned (#5167 review: "Scan tail heredocs in terminal compounds").
func binaryTailHasHeredoc(bin *syntax.BinaryCmd, command string) bool {
	tail := binaryTailStmt(bin)
	if tail == nil {
		return false
	}
	return stmtHasHeredocRedirect(tail, command)
}

// stmtTrailingComment reports whether a `#` shell comment on stmt follows the
// last word of words and is the final content on the last line of command. It is
// the shared scan behind CommandHasTrailingComment (a single simple call) and
// the binary/multi-statement tail checks. A trailing newline after the comment
// starts a new statement that carries the flag, which CommandHasControlOperator
// already warns about, so a comment is only a misroute when nothing follows it
// on the last line.
func stmtTrailingComment(stmt *syntax.Stmt, words []*syntax.Word, command string) bool {
	if len(words) == 0 || len(stmt.Comments) == 0 {
		return false
	}
	if hasTrailingNewline(command) {
		return false
	}
	lastEnd := words[len(words)-1].End().Offset()
	for _, cm := range stmt.Comments {
		if cm.Hash.Offset() > lastEnd {
			return true
		}
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
	words, _ := stripExecPrefix(call.Args)
	return stmtTrailingComment(stmt, words, command)
}

// stmtHasHeredocRedirect reports whether stmt carries a here-document redirect
// (`<<`/`<<-`) whose body is the last content on the last line of command. It is
// the shared scan behind CommandHasHeredoc (a single simple call) and the
// binary/multi-statement tail checks. A trailing newline after the closing
// delimiter starts a new statement that carries the flag, which
// CommandHasControlOperator already warns about, so a heredoc is only a misroute
// when its body is the final content on the last line.
func stmtHasHeredocRedirect(stmt *syntax.Stmt, command string) bool {
	if hasTrailingNewline(command) {
		return false
	}
	for _, r := range stmt.Redirs {
		if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc {
			return true
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
	return stmtHasHeredocRedirect(stmt, command)
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
	if backs%2 == 0 {
		return true
	}
	// An odd run before the newline is a line continuation, except when the
	// backslashes are inside a `#` comment: a comment ends at the newline, so
	// the backslash cannot continue the line and the newline is a real
	// statement terminator (#5167 review: "Do not treat comment backslashes as
	// line continuations").
	return trailingBackslashRunInComment(command, len(trimmed)-1, backs)
}
