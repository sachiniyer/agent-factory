package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

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
// The forwarding exemption holds only when a dummy `$0` placeholder precedes
// the forwarded agent. The operand right after the `-c` script is the shell's
// `$0`, so a wrapper that puts the agent there (`sh -c 'exec "$@"' claude`)
// assigns `claude` to `$0` and `"$@"` then begins with the appended flag, so
// `exec` runs the flag rather than claude. A placeholder before the agent
// (`sh -c 'exec "$@"' sh claude`) makes the agent `$1` and `"$@"` runs it with
// the flag, so only that shape is exempt (#5167 review: "Require a `$0`
// placeholder before exempting forwarding shells"). `agent` is the agent
// injectSystemPrompt detects in the value (lowercased basename), the same
// `CommandHasControlOperator` receives; "" preserves the prior exempt shape
// when no agent is detected.
func CommandInvokesAgentViaInterpreter(command, agent string) bool {
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		return false
	}
	words, _ := stripExecPrefix(call.Args)
	// `exec` carries its own options before the command it runs — bash's
	// `help exec` documents `exec [-cl] [-a name] [command [argument ...]]` —
	// and stripExecPrefix consumes only the `exec` keyword and an optional
	// `--`, so `exec -a af /bin/sh -c 'claude'` leaves `-a` as the first word
	// and the known-shell check below returns false. Injection then yields
	// `exec -a af /bin/sh -c claude --plugin-dir …`, where the inner shell
	// assigns the appended flag to `$0` instead of passing it to claude, so
	// the plugin is silently lost. Peel exec's own options so the interpreter
	// — not the option that names it — is what the shell check runs on
	// (#5167 review: "Parse `exec` options before checking interpreter
	// wrappers").
	words = skipExecOptions(words)
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
	//
	// A script that forwards its positionals to the command it invokes, however,
	// carries the appended flag — now a positional to the interpreter — through
	// to the agent: `sh -c 'exec "$@"' sh claude` becomes
	// `sh -c 'exec "$@"' sh claude --plugin-dir …` and `"$@"` runs claude with
	// `--plugin-dir`, so claude starts WITH the plugin and the warning would be a
	// false positive. The word after the `-c` flag (or a cluster carrying it) is
	// the script; a script that references the positional parameters (`$@`) is
	// treated as a forwarding wrapper and not warned (#5167 review: "Avoid
	// warning for forwarding `sh -c` wrappers"). A non-literal script (expansions
	// inside double quotes) is not recognized and stays warned, the safe
	// direction; a script that names the agent without forwarding keeps the flag
	// from it and is still warned.
	for i := 1; i < len(words); i++ {
		lit, ok := literalShellWord(words[i])
		if !ok {
			continue
		}
		if lit == "-c" || shellShortClusterHasC(lit) {
			if i+1 < len(words) && scriptForwardsPositionals(words[i+1], agent) {
				// A script that runs the forwarded argv as the command
				// (`exec "$@"`) needs a `$0` placeholder before the agent:
				// without one the agent is `$0` and `"$@"` runs the appended
				// flag rather than the agent. A script that names the agent
				// itself with `"$@"` as an argument (`claude "$@"`) invokes the
				// agent directly, so `$0` is irrelevant and no placeholder is
				// needed (#5167 review: "Do not warn for hard-coded `$@`
				// forwarding without `$0`").
				if scriptForwardsArgvAsCommand(words[i+1]) && !forwardingScriptHasArgvPlaceholder(words, i, agent) {
					return true
				}
				return false
			}
			return true
		}
	}
	return false
}

// forwardingScriptHasArgvPlaceholder reports whether the operands after a `-c`
// script put a dummy `$0` before the forwarded agent, the shape that lets
// `"$@"` run the agent with af's appended flag instead of running the flag
// itself. cIndex is the index of the `-c` flag (or a cluster carrying it);
// the script is at cIndex+1, so the `$0` operand is at cIndex+2. With no
// operand after the script there is nothing to misroute (the no-operand
// pass-through shape), and with no detected agent there is no agent to place
// at `$0`, so both keep the prior exempt behavior.
func forwardingScriptHasArgvPlaceholder(words []*syntax.Word, cIndex int, agent string) bool {
	if agent == "" {
		return true
	}
	if cIndex+2 >= len(words) {
		return true
	}
	return firstWordAgentName(words[cIndex+2:cIndex+3]) != agent
}

// scriptForwardsArgvAsCommand reports whether the `-c` script's last statement
// runs the forwarded positional argv as the command itself (`exec "$@"` or
// `"$@"`), the forwarding shape that needs a `$0` placeholder before the agent
// so `"$@"` runs the agent with the appended flag rather than running the flag
// itself. A script that names the agent with `"$@"` as an argument
// (`claude "$@"`) invokes the agent directly and does not need a placeholder,
// so it is not this shape and the placeholder check does not apply
// (#5167 review: "Do not warn for hard-coded `$@` forwarding without `$0`").
func scriptForwardsArgvAsCommand(script *syntax.Word) bool {
	lit, ok := literalShellWord(script)
	if !ok {
		return false
	}
	if !strings.Contains(lit, "$@") {
		return false
	}
	stmt, ok := lastScriptStmt(lit)
	if !ok {
		return false
	}
	return stmtRunsArgvAsCommand(stmt)
}

// stmtRunsArgvAsCommand reports whether stmt runs the positional argv `$@` as
// its command (after an optional leading `exec`), the shape that needs a `$0`
// placeholder before the forwarded agent. It is the command-position scan behind
// scriptForwardsArgvAsCommand, mirroring the first branch of callForwardsArgv.
func stmtRunsArgvAsCommand(stmt *syntax.Stmt) bool {
	if stmt == nil {
		return false
	}
	switch c := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return stmtRunsArgvAsCommand(c.X) || stmtRunsArgvAsCommand(c.Y)
	case *syntax.CallExpr:
		return callRunsArgvAsCommand(c.Args)
	}
	return false
}

// callRunsArgvAsCommand reports whether the words of a simple call run the
// forwarded positional argv as the command, i.e. the command word (after an
// optional leading `exec`) is `$@`/`"$@"`. It is the call-level scan behind
// stmtRunsArgvAsCommand; a call that names the agent with `$@` as an argument
// (`claude "$@"`) does not run the positionals as the command and is not this
// shape.
func callRunsArgvAsCommand(args []*syntax.Word) bool {
	if len(args) == 0 {
		return false
	}
	cmdArgs := args
	if lit, ok := literalShellWord(args[0]); ok && lit == "exec" {
		cmdArgs = args[1:]
		if len(cmdArgs) == 0 {
			return false
		}
	}
	return wordReferencesAtParam(cmdArgs[0])
}

// scriptForwardsPositionals reports whether the `-c` script word forwards its
// positional parameters to the command it invokes — the shape that carries af's
// appended flag, a positional to the interpreter, through to the agent. An
// `exec "$@"` (or any `"$@"`/`$@` reference that invokes the forwarded command)
// runs the command named in the positionals with those positionals as arguments,
// so `sh -c 'exec "$@"' sh claude` becomes `sh -c 'exec "$@"' sh claude --plugin-dir …`
// and claude receives `--plugin-dir` (#5167 review: "Avoid warning for forwarding
// `sh -c` wrappers"). Only `"$@"`/`$@` preserve the appended flag as separate argv
// entries; quoted `"$*"` collapses the positionals into one word (so
// `sh -c 'exec "$*"' sh claude` becomes one program name like
// `claude --plugin-dir /path` and fails to launch), and unquoted `$*` is
// IFS-dependent, so `$*` is NOT treated as forwarding and stays warned (#5167
// review: "Do not treat quoted `$*` as argv forwarding"). The canonical
// forwarding form is single-quoted, so it reaches this check as one literal; a
// non-literal script is not recognized and stays warned, which is the safe
// direction.
//
// A bare `strings.Contains(lit, "$@")` would also exempt a script that merely
// consumes the positionals in an earlier statement and then runs the agent
// without them: `sh -c 'echo "$@"; claude' sh claude` lets `echo` consume the
// positionals and the hard-coded `claude` start without the flag, yet the
// substring test returned true. The exemption is restricted to a positional
// expansion in the LAST statement of the script, the command that runs last and
// the one the appended flag must reach for the agent to receive it; an earlier
// statement that consumes `$@` does not forward it to the agent (#5167 review:
// "Only exempt scripts that actually forward positional argv"). The final
// statement must also INVOKE the forwarded argv — `exec "$@"` or `"$@"` — or
// run the agent itself with `$@` among its arguments — `claude "$@"`; a call
// that merely consumes `$@` in an argument (`echo "$@"`) prints the positionals
// and exits without launching the agent, so it does not forward and stays
// warned (#5167 review: "Verify `$@` actually launches the forwarded command").
func scriptForwardsPositionals(script *syntax.Word, agent string) bool {
	lit, ok := literalShellWord(script)
	if !ok {
		return false
	}
	if !strings.Contains(lit, "$@") {
		return false
	}
	// The script is the literal `-c` argument, so it parses as a command of its
	// own; the appended flag is a positional to the shell, available to every
	// statement, so the last statement that references `$@` is the one whose
	// command receives (or execs) the forwarded positionals. A script that does
	// not parse is left to the safe substring fallback.
	if stmt, ok := lastScriptStmt(lit); ok {
		return stmtReferencesAtParam(stmt, agent)
	}
	return true
}

// lastScriptStmt parses the literal text of a `-c` script and returns its last
// statement, the command that the appended positional reaches when the script
// runs. It is the shared scan behind scriptForwardsPositionals' forwarding
// check: a `$@` in an earlier statement is consumed there and does not reach the
// last command, so only the last statement decides whether the positionals are
// forwarded to the agent.
func lastScriptStmt(script string) (*syntax.Stmt, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "")
	if err != nil || file == nil || len(file.Stmts) == 0 {
		return nil, false
	}
	return file.Stmts[len(file.Stmts)-1], true
}

// stmtReferencesAtParam reports whether stmt invokes the positional parameter
// `$@` (a ParamExp whose parameter is `@`, bare or inside double quotes) as the
// command that receives af's appended flag — the shape that forwards the
// interpreter's positionals through to the agent. `$@` is the only expansion
// that preserves the appended flag as separate argv entries; `$*` is not
// forwarding and is excluded by the parameter name check. The final statement
// forwards only when it RUNS the forwarded argv: the command word (after an
// optional `exec`) is itself `$@` (`exec "$@"`, `"$@"`), or the command is the
// detected agent with `$@` among its arguments (`claude "$@"`). A call that
// only consumes `$@` in an argument — `echo "$@"` — prints the positionals and
// exits without launching the agent, so it does not forward and stays warned
// (#5167 review: "Verify `$@` actually launches the forwarded command").
// Compound constructs (subshell, if/for/while) are not analyzed and report
// false, the conservative answer for the forwarding exemption — a compound
// that hides a `$@` is not the plain forwarding shape this guard exempts.
func stmtReferencesAtParam(stmt *syntax.Stmt, agent string) bool {
	if stmt == nil {
		return false
	}
	switch c := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return stmtReferencesAtParam(c.X, agent) || stmtReferencesAtParam(c.Y, agent)
	case *syntax.CallExpr:
		return callForwardsArgv(c.Args, agent)
	}
	return false
}

// callForwardsArgv reports whether the words of a simple call invoke the
// forwarded positional argv so the appended flag reaches the agent. It is the
// call-level scan behind stmtReferencesAtParam. A leading literal `exec` runs
// the remaining words as the command, so it is peeled before the command
// position is examined: `exec "$@"` forwards because the command (after exec)
// is `$@`, and `exec claude "$@"` forwards because the command is the agent
// with `$@` among its arguments. `echo "$@"` does not forward: echo is neither
// `$@` nor the agent, so the positionals are merely printed.
func callForwardsArgv(args []*syntax.Word, agent string) bool {
	if len(args) == 0 {
		return false
	}
	cmdArgs := args
	if lit, ok := literalShellWord(args[0]); ok && lit == "exec" {
		cmdArgs = args[1:]
		if len(cmdArgs) == 0 {
			return false
		}
	}
	// The command position is the forwarded argv itself: `"$@"` or `exec "$@"`.
	if wordReferencesAtParam(cmdArgs[0]) {
		return true
	}
	// The command is the detected agent and `$@` is among its arguments, so the
	// appended flag reaches the agent through that argument: `claude "$@"`.
	if agent != "" && firstWordAgentName(cmdArgs) == agent {
		for _, w := range cmdArgs[1:] {
			if wordReferencesAtParam(w) {
				return true
			}
		}
	}
	return false
}

// wordReferencesAtParam reports whether word holds a `$@` parameter expansion,
// bare (`$@`) or quoted (`"$@"`), at any depth of double quoting. It is the
// word-level scan behind stmtReferencesAtParam.
func wordReferencesAtParam(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		if wordPartReferencesAtParam(part) {
			return true
		}
	}
	return false
}

func wordPartReferencesAtParam(part syntax.WordPart) bool {
	switch p := part.(type) {
	case *syntax.ParamExp:
		return p.Param != nil && p.Param.Value == "@"
	case *syntax.DblQuoted:
		for _, nested := range p.Parts {
			if wordPartReferencesAtParam(nested) {
				return true
			}
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
// flags span every letter `bash --help` lists as argument-free shell options
// (`-abefhkmptuvxBCEHPT` and `-ilrsD`) except `-n` (noexec, which prevents the
// script from running at all, so the appended flag cannot misroute it); `-e`,
// `-u`, and `-x` were the first three added (#5167 review: "Recognize `-e`/`-u`/
// `-x` in shell `-c` option clusters"), and the rest complete the set so a
// cluster such as `bash -vc 'claude'` is recognized the same way (#5167 review:
// "Scan all argument-free shell flags before `-c`"). A `c` after any other flag
// is not claimed as the script flag, so an unknown option stays conservative
// rather than over-warning.
func shellShortClusterHasC(word string) bool {
	if len(word) <= 2 || word[0] != '-' || word[1] == '-' {
		return false
	}
	for _, flag := range word[1:] {
		switch flag {
		case 'c':
			return true
		case 'a', 'b', 'e', 'f', 'h', 'i', 'k', 'l', 'm', 'p', 'r', 's',
			't', 'u', 'v', 'x', 'D', 'B', 'C', 'E', 'H', 'P', 'T':
			continue
		default:
			return false
		}
	}
	return false
}

// skipExecOptions drops a leading `exec` builtin's own options — the `-c`
// and `-l` argument-free flags, the `-a name` form that sets argv[0], and the
// `--` that ends them — so the interpreter check locates the command `exec`
// runs rather than the option that names it. Bash's `help exec` documents
// `exec [-cl] [-a name] [command [argument ...]]`: `-c` and `-l` take no
// argument, `-a` consumes the next word as the argv[0] name, and `--` ends
// the options. Without this peel, `exec -a af /bin/sh -c 'claude'` leaves
// `-a` as the first word, so no interpreter wrapper is recognized even though
// injection yields `exec -a af /bin/sh -c claude --plugin-dir …` and the
// inner shell assigns the appended flag to `$0`, silently losing the af
// plugin (#5167 review: "Parse `exec` options before checking interpreter
// wrappers").
//
// Scoped to the interpreter-wrapper warning rather than the shared
// stripExecPrefix tokenizer: the account boundary's refusal of `exec --`
// turns on portability across /bin/sh implementations, and consuming `-a`
// there would change which shapes it refuses, which is outside this
// warning's diff. A cluster the documented grammar does not cover — an
// attached `-a` argument such as `-afoo`, an `a` that is not the last flag,
// or any unknown flag — stops the peel so the prior no-warning behavior
// stays, the safe direction for a warning predicate.
func skipExecOptions(words []*syntax.Word) []*syntax.Word {
	for len(words) > 0 {
		lit, ok := literalShellWord(words[0])
		if !ok {
			return words
		}
		if lit == "--" {
			return words[1:]
		}
		if len(lit) < 2 || lit[0] != '-' || lit[1] == '-' {
			return words
		}
		flags := lit[1:]
		aIndex := strings.IndexByte(flags, 'a')
		switch {
		case aIndex < 0:
			// `-c`, `-l`, or a cluster of only those flags: all argument-free.
			if !onlyExecFlags(flags) {
				return words
			}
			words = words[1:]
		case aIndex == len(flags)-1:
			// `a` is the last flag, so the next word is the argv[0] name; the
			// flags before it must be only `c`/`l`. Both words are dropped.
			if aIndex > 0 && !onlyExecFlags(flags[:aIndex]) {
				return words
			}
			if len(words) < 2 {
				return words[1:]
			}
			words = words[2:]
		default:
			// `a` is not last (an attached-arg form such as `-afoo`): the
			// documented grammar does not cover it, so stop conservatively.
			return words
		}
	}
	return words
}

// onlyExecFlags reports whether flags contains only the argument-free `exec`
// options `c` and `l`. An empty flags string is true so the bare `-a` form
// (no preceding flags) peels its name argument.
func onlyExecFlags(flags string) bool {
	for _, r := range flags {
		if r != 'c' && r != 'l' {
			return false
		}
	}
	return true
}
