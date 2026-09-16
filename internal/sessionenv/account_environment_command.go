package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// commandMutatesAccountEnvironment recognizes command-local mutations that can
// replace a sibling tab's selected account. Unlike commandOverridesName, this
// path launches arbitrary user processes, so unrelated configuration such as
// PORT=3000 remains allowed. Shell decorations do not hide calls from the walk,
// and an unsupported form of a recognized environment mutator fails closed.
func commandMutatesAccountEnvironment(command string, names map[string]struct{}) bool {
	return commandEnvironmentWalkMutates(command, names, nil)
}

// commandAdmitsZshLaunch reports whether any position the command validator
// judges resolves to the admitted zsh launch: a top-level call after wrapper
// removal (`nice /bin/zsh -f -i`), a compound-list sibling (`/bin/zsh -f -i;
// true`), env's command word, or a shell word inside an unrecognized
// wrapper's argv. isGeneratedAccountZsh saw only the exact generated
// spelling, so those admitted forms ran zsh with ZDOTDIR merely unset — where
// a `setopt RCS` in /etc/zsh/zshenv re-admits the user startup chain that can
// rewrite the account root (Codex on #4474). The pin this feeds still lands
// only on commands that can execute zsh: `echo /bin/zsh` never reaches a
// judged zsh position, and unrelated surfaces such as code-server and login
// panes keep their unpinned environment (#4474 review).
func commandAdmitsZshLaunch(command string, names map[string]struct{}) bool {
	launchesZsh := false
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			continue
		}
		syntax.Walk(file, func(node syntax.Node) bool {
			if launchesZsh {
				return false
			}
			// The verdict is deliberately unused: the traversal itself is what
			// visits every judged command position and records a proven zsh.
			nodeMutatesAccountEnvironment(node, names, &launchesZsh)
			return true
		})
		if launchesZsh {
			return true
		}
	}
	return false
}

func commandEnvironmentWalkMutates(command string, names map[string]struct{}, launchesZsh *bool) bool {
	if command == "" {
		return false
	}
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			return true
		}
		mutates := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if nodeMutatesAccountEnvironment(node, names, launchesZsh) {
				mutates = true
				return false
			}
			return true
		})
		if mutates {
			return true
		}
	}
	return false
}

func nodeMutatesAccountEnvironment(node syntax.Node, names map[string]struct{}, launchesZsh *bool) bool {
	switch node := node.(type) {
	case *syntax.CallExpr:
		return callMutatesAccountEnvironment(node, names, launchesZsh)
	case *syntax.Assign:
		return node.Name != nil && accountEnvironmentNameDenied(node.Name.Value, names)
	case *syntax.WordIter:
		return node.Name != nil && accountEnvironmentNameDenied(node.Name.Value, names)
	case *syntax.ParamExp:
		return node.Param != nil && node.Exp != nil &&
			(node.Exp.Op == syntax.AssignUnset || node.Exp.Op == syntax.AssignUnsetOrNull) &&
			accountEnvironmentNameDenied(node.Param.Value, names)
	case *syntax.BinaryArithm:
		return arithmeticAssignmentMutatesAccountEnvironment(node, names)
	case *syntax.UnaryArithm:
		return arithmeticIncrementMutatesAccountEnvironment(node, names)
	case *syntax.UnaryTest:
		return unaryTestMutatesAccountEnvironment(node)
	default:
		return false
	}
}

func accountEnvironmentNameDenied(name string, names map[string]struct{}) bool {
	_, denied := names[name]
	return denied
}

func arithmeticAssignmentMutatesAccountEnvironment(expr *syntax.BinaryArithm, names map[string]struct{}) bool {
	switch expr.Op {
	case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn,
		syntax.QuoAssgn, syntax.RemAssgn, syntax.AndAssgn, syntax.OrAssgn,
		syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn, syntax.AndBoolAssgn,
		syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
		return arithmeticTargetMutates(expr.X, names)
	default:
		return false
	}
}

func arithmeticIncrementMutatesAccountEnvironment(expr *syntax.UnaryArithm, names map[string]struct{}) bool {
	if expr.Op != syntax.Inc && expr.Op != syntax.Dec {
		return false
	}
	return arithmeticTargetMutates(expr.X, names)
}

// arithmeticTargetMutates decides an arithmetic assignment or increment by its
// TARGET, and fails closed when it cannot read that target literally.
//
// Reading it as "not literal, therefore not a denied name" was the hole:
// `CODEX_HOME[0]` is an indexed-array element, which this parser reports as a
// non-literal word, so an assignment straight at the protected name scored as
// safe. bash applies `(( CODEX_HOME[0]=1 ))` to the exported SCALAR and
// converts it to an indexed array — after which a child reads CODEX_HOME as
// EMPTY, so the selected account root is destroyed rather than merely
// replaced. A dynamic target such as `${name}` is unprovable for the same
// reason.
//
// accountEnvironmentOperandDenied already fails closed on any subscript it CAN
// read; this is the same rule for the ones it cannot.
func arithmeticTargetMutates(target syntax.ArithmExpr, names map[string]struct{}) bool {
	name, ok := arithmeticAccountEnvironmentName(target)
	if !ok {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func arithmeticAccountEnvironmentName(expr syntax.ArithmExpr) (string, bool) {
	word, ok := expr.(*syntax.Word)
	if !ok {
		return "", false
	}
	return literalShellWord(word)
}

func callMutatesAccountEnvironment(call *syntax.CallExpr, names map[string]struct{}, launchesZsh *bool) bool {
	for _, assign := range call.Assigns {
		if assign != nil && assign.Name != nil {
			if _, denied := names[assign.Name.Value]; denied {
				return true
			}
		}
	}

	words, unsafe := unwrapAccountCommand(call.Args, names, launchesZsh)
	if unsafe || len(words) == 0 {
		return unsafe
	}
	return unwrappedAccountCommandMutates(words, names, launchesZsh)
}

func unwrappedAccountCommandMutates(words []*syntax.Word, names map[string]struct{}, launchesZsh *bool) bool {
	if _, literal := literalShellWord(words[0]); !literal {
		// A dynamic command name can resolve to env or a same-shell builtin such
		// as unset/export, so its effect on the selected identity is unprovable.
		return true
	}
	switch {
	case isAccountCommandName(words[0], "env"):
		return envCallMutatesAccountEnvironment(words[1:], names, false, launchesZsh)
	case isBareName(words[0], "unset"):
		return unsetMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "set"):
		return setMutatesAccountEnvironment(words[1:])
	case isBareName(words[0], "hash"):
		return hashMutatesAccountEnvironment(words[1:])
	case isAccountDeclarationBuiltin(words[0]):
		return declarationMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "read"):
		return readMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "getopts"):
		return getoptsMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "printf"):
		return printfMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "let"):
		return letMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "mapfile"), isBareName(words[0], "readarray"):
		return arrayReadMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "wait"):
		return waitMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "test"), isBareName(words[0], "["):
		return variableTestMutatesAccountEnvironment(words[1:])
	case isBareName(words[0], "eval"), isBareName(words[0], "."),
		isBareName(words[0], "source"), isBareName(words[0], "trap"),
		isBareName(words[0], "alias"), isBareName(words[0], "fc"),
		isBareName(words[0], "history"), isBareName(words[0], "enable"):
		// These builtins execute or schedule another shell program in the same
		// environment, define commands that do, or load/replay unparsed history.
		// Proving their effects would require a second parser pass with runtime
		// expansion, so a scoped sibling refuses them.
		return true
	case shellCommandIsUnproven(words, launchesZsh):
		return true
	default:
		return false
	}
}

// unwrapAccountCommand removes shell wrappers that still execute the remaining
// words in the current environment. Dynamic or unsupported wrapper forms are
// unsafe because they could resolve to env or an environment-mutating builtin.
func unwrapAccountCommand(words []*syntax.Word, names map[string]struct{}, launchesZsh *bool) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		switch {
		case isBareName(words[0], "exec"):
			words = words[1:]
			if len(words) > 0 {
				option, literal := literalShellWord(words[0])
				if !literal {
					return nil, true
				}
				if option == "--" {
					words = words[1:]
				} else if strings.HasPrefix(option, "-") && option != "-" {
					// Bash and other shells give exec options environment-changing
					// behavior (notably `exec -c`). No option is needed by af's
					// sibling path, so unsupported forms fail closed.
					return nil, true
				}
			}
			for len(words) > 0 {
				name, assignment := shellWordAssignmentName(words[0])
				if !assignment {
					break
				}
				if _, denied := names[name]; denied {
					return nil, true
				}
				words = words[1:]
			}
		case isBareName(words[0], "command"):
			var unsafe bool
			words, unsafe = unwrapCommandBuiltin(words[1:])
			if unsafe {
				return nil, true
			}
		case isBareName(words[0], "builtin"):
			words = words[1:]
			if len(words) > 0 && wordEquals(words[0], "--") {
				words = words[1:]
			}
			if len(words) > 0 {
				if _, literal := literalShellWord(words[0]); !literal {
					return nil, true
				}
			}
		case isAccountCommandName(words[0], "nohup"):
			var unsafe bool
			words, unsafe = unwrapNohup(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "nice"):
			var unsafe bool
			words, unsafe = unwrapNice(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "timeout"):
			var unsafe bool
			words, unsafe = unwrapTimeout(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "setsid"):
			var unsafe bool
			words, unsafe = unwrapSetsid(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "stdbuf"):
			var unsafe bool
			words, unsafe = unwrapStdbuf(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "ionice"):
			var unsafe bool
			words, unsafe = unwrapIonice(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "taskset"):
			var unsafe bool
			words, unsafe = unwrapTaskset(words[1:])
			if unsafe {
				return nil, true
			}
		case isAccountCommandName(words[0], "xargs"):
			var unsafe bool
			words, unsafe = unwrapXargs(words[1:], names)
			if unsafe {
				return nil, true
			}
		default:
			if unrecognizedWrapperHidesAccountAssignment(words, names, launchesZsh) {
				return nil, true
			}
			return words, false
		}
	}
	return nil, false
}

// unrecognizedWrapperHidesAccountAssignment reports whether the literal tail
// words of an unrecognized argv-passthrough wrapper carry a NAME=... assignment
// whose NAME is one af removes for the selected account, specifically by
// nesting an env invocation inside the wrapper's argument list.
//
// unwrapAccountCommand peels a CLOSED list of wrappers (exec/command/builtin/
// nohup/nice/timeout/setsid/stdbuf/ionice/taskset/xargs); every other binary
// that runs a child command and passes argv through (strace/perf/valgrind/
// gdb --args/...) falls to the default arm and was returned opaque. Wrapping the
// modelled `env NAME=value <agent>` mutation — which the guard already refuses
// bare and under every modelled wrapper — in an unmodeled wrapper hid the inner
// assignment from the walk, so `strace env CODEX_HOME=/other codex` was accepted
// while `nohup env CODEX_HOME=/other codex` was refused.
//
// This lifts envCallMutatesAccountEnvironment's NAME= rule one level: scan
// the wrapper's literal argv tail for an `env` invocation and delegate to
// envCallMutatesAccountEnvironment when one is found. Commands that do not
// contain a nested env invocation are not refused even when an argument
// resembles a NAME=value token, so noun-uses such as `echo CODEX_HOME=/tmp`,
// `rg 'OPENAI_API_KEY='`, `man env`, `make env`, `git grep env`, `ls env/bin`,
// `pip show env`, or `strace -p 1234 env` (none of which carries a nested env
// invocation with a denied assignment) stay allowed.
//
// The same tail scan applies the modeled path's other two rules one level
// down: a shell word with argv is judged by shellCommandIsUnproven exactly as
// a bare `sh -c '...'` command is, and a literal option word whose VALUE is a
// denied name or assignment (xargs --process-slot-var=NAME, strace -E var=val
// and --env=var=val) is refused — a wrapper's own options can place the
// mutation without any env word.
func unrecognizedWrapperHidesAccountAssignment(words []*syntax.Word, names map[string]struct{}, launchesZsh *bool) bool {
	strace := isAccountCommandName(words[0], "strace")
	for i := 1; i < len(words); i++ {
		word := words[i]
		if isAccountCommandName(word, "env") {
			// A nested env only mutates the child it execs; requireCommand
			// keeps an `env CODEX_HOME=/tmp` whose argv ends there — env's
			// print mode, which overrides nothing — allowed.
			if envCallMutatesAccountEnvironment(words[i+1:], names, true, launchesZsh) {
				return true
			}
			continue
		}
		literal, ok := literalShellWord(word)
		if !ok {
			// An unprovable tail word can itself expand to `env` (or to a
			// multiword `env NAME=value` after word splitting); judge the
			// words after it as that invocation's argv.
			if envCallMutatesAccountEnvironment(words[i+1:], names, true, launchesZsh) {
				return true
			}
			continue
		}
		if !strings.HasPrefix(literal, "-") {
			// A shell in the wrapper's tail gets the same verdict a bare
			// shell command gets: `strace sh -c 'unset CODEX_HOME; codex'`
			// execs the literal script the modeled path already refuses
			// under `nice sh -c ...`. Only the trusted account-shell form
			// proves out. A trailing shell name with no argv (echo sh,
			// strace -p 1 sh) has nothing to judge and stays allowed.
			if i+1 < len(words) && knownShellName(filepath.Base(literal)) &&
				shellCommandIsUnproven(words[i:], launchesZsh) {
				return true
			}
			continue
		}
		if strace {
			switch {
			case literal == "-E" || literal == "--env":
				// strace's env option takes var[=val] as a separate word and
				// injects or REMOVES the variable in the traced child's
				// environment — the same mutation the env arm refuses, in
				// option spelling. It is strace-only because -E means
				// extended-regexp to grep and friends.
				i++
				if i >= len(words) {
					return true
				}
				value, ok := literalShellWord(words[i])
				if !ok || accountEnvironmentOperandDenied(value, names) {
					return true
				}
				continue
			case strings.HasPrefix(literal, "-E"):
				if accountEnvironmentOperandDenied(literal[2:], names) {
					return true
				}
				continue
			}
		}
		// An unrecognized wrapper may expose options that mutate its
		// child's environment in option-value form — xargs's
		// --process-slot-var=NAME sets NAME on every exec'd command, and
		// strace's --env=var=val is analogous. A literal `--opt=DENIED` or
		// `--opt=DENIED=value` is refused when its value names a denied
		// variable or carries a denied assignment.
		if _, value, ok := strings.Cut(literal, "="); ok {
			if accountEnvironmentOperandDenied(value, names) {
				return true
			}
		}
	}
	return false
}

func variableTestMutatesAccountEnvironment(words []*syntax.Word) bool {
	for idx := 0; idx < len(words); idx++ {
		option, literal := literalShellWord(words[idx])
		if !literal {
			return true
		}
		if option != "-v" {
			continue
		}
		if idx+1 >= len(words) {
			return true
		}
		operand, literal := literalShellWord(words[idx+1])
		if !literal || strings.Contains(operand, "[") {
			return true
		}
		idx++
	}
	return false
}

func unaryTestMutatesAccountEnvironment(test *syntax.UnaryTest) bool {
	if test.Op != syntax.TsVarSet {
		return false
	}
	word, ok := test.X.(*syntax.Word)
	if !ok {
		return true
	}
	operand, literal := literalShellWord(word)
	return !literal || strings.Contains(operand, "[")
}

func unwrapCommandBuiltin(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			words = words[1:]
			break
		}
		if !strings.HasPrefix(option, "-") || option == "-" {
			break
		}
		for _, flag := range option[1:] {
			switch flag {
			case 'p':
			case 'v', 'V':
				return nil, false // query-only: command does not execute its operand
			default:
				return nil, true
			}
		}
		words = words[1:]
	}
	if len(words) > 0 {
		if _, literal := literalShellWord(words[0]); !literal {
			return nil, true
		}
	}
	return words, false
}

// envCallArgvParse literalizes env's operand words the way env itself parses
// them: a non-literal word that still spells a NAME= assignment keeps its name
// (the value is dynamic), and anything else is unprovable so the parse fails.
func envCallArgvParse(words []*syntax.Word) (envcommand.Invocation, error) {
	literals := make([]string, 0, len(words))
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			name, assignment := shellWordAssignmentName(word)
			if !assignment {
				return envcommand.Invocation{}, envcommand.ErrUnsupported
			}
			value = name + "=AF_DYNAMIC_VALUE"
		}
		literals = append(literals, value)
	}
	return envcommand.Parse(literals, envcommand.Policy{AllowAssignments: true})
}

// envCallMutatesAccountEnvironment reports whether the env invocation formed by
// words mutates a denied name or execs something unprovable. requireCommand
// restricts that verdict to env invocations carrying a command word: print-mode
// env mutates only its own process, which is the right reading when the words
// sit in an unrecognized wrapper's tail rather than forming the command itself —
// an `env CODEX_HOME=/tmp` with nothing after it runs env's print path, not an
// override of a running agent.
func envCallMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}, requireCommand bool, launchesZsh *bool) bool {
	invocation, err := envCallArgvParse(words)
	if err != nil || invocation.ClearEnvironment {
		return true
	}
	if requireCommand && invocation.CommandIndex < 0 {
		return false
	}
	for _, mutation := range invocation.Mutations {
		if _, denied := names[mutation.Name]; denied {
			return true
		}
	}
	if invocation.CommandIndex >= 0 {
		commandWords, unsafe := unwrapAccountCommand(words[invocation.CommandIndex:], names, launchesZsh)
		if unsafe {
			return true
		}
		if len(commandWords) == 0 {
			return false
		}
		return unwrappedAccountCommandMutates(commandWords, names, launchesZsh)
	}
	return false
}

func shellCommandIsUnproven(words []*syntax.Word, launchesZsh *bool) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	if !literal || !knownShellName(filepath.Base(command)) {
		return false
	}
	proven := accountShellCommandWordsProven(words)
	if proven && filepath.Base(command) == "zsh" && launchesZsh != nil {
		// A judged command position resolved to the admitted zsh launch —
		// top-level call, wrapper tail, env's command word, or a shell word
		// inside an unrecognized wrapper's argv. Unproven zsh forms are
		// refused by this same check, so the flag names exactly the
		// executions the ZDOTDIR pin exists to cover (Codex on #4474).
		*launchesZsh = true
	}
	return !proven
}

func accountShellCommandWordsProven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, _ := literalShellWord(words[0])
	// A sibling shell may read profiles, stdin, a script, or a command string.
	// The only statically proven form is the same absolute, startup-free command
	// AccountShellCommand generates for a dedicated shell tab.
	args, literal := literalCommandArgs(words)
	if !literal || !filepath.IsAbs(command) {
		return false
	}
	want := trustedAccountShellArgs(command)
	return want != nil && slices.Equal(args[1:], want)
}

func knownShellName(name string) bool {
	switch name {
	case "ash", "bash", "csh", "dash", "fish", "ksh", "mksh", "sh", "tcsh", "zsh":
		return true
	default:
		return false
	}
}

func isAccountCommandName(word *syntax.Word, want string) bool {
	value, literal := literalShellWord(word)
	return literal && filepath.Base(value) == want
}

func unsetMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	functionsOnly := false
	options := true
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-f":
				functionsOnly = true
				continue
			case "-v":
				functionsOnly = false
				continue
			}
			if strings.HasPrefix(value, "-") {
				return true
			}
			options = false
		}
		if !functionsOnly {
			if accountEnvironmentOperandDenied(value, names) {
				return true
			}
		}
	}
	return false
}

// setMutatesAccountEnvironment reports whether a `set` call switches the shell
// into keyword mode, where an assignment-shaped word written AFTER a command
// name is placed in that command's environment instead of staying an argument.
//
// This walk reads every later call under DEFAULT parsing rules, so the mode is
// not a mutation of its own — it silently invalidates every verdict that
// follows it. Under `set -k`, `codex CODEX_HOME=/other` is not the two-word
// call this walk sees: bash removes the assignment from codex's arguments and
// launches it with the replacement root. Refusing the switch is what keeps the
// rest of the walk meaningful; tracking the mode across calls instead would
// have to model the shell's own state machine.
//
// Deliberately narrow: a process tab runs an arbitrary user command, and an
// ordinary `set -e` prologue must keep working. Only keyword mode is refused.
//
// Option arity modelled by this scanner:
//
//	-o / +o   conditional arity — consumes the next word as a mode name ONLY
//	          when that word does not start with `-` or `+`. Real mode names
//	          (pipefail, noclobber, keyword, …) never start with either; when
//	          the next word does start with one it is another option that the
//	          scan must keep examining. This applies to both the standalone
//	          `-o` word and to `o` embedded in a minus-prefixed cluster.
//	          These are the only conditional-arity options; all others have
//	          fixed arity (zero).
//
// Keyword-mode tracking: bash processes options left to right; a later `-k`
// overrides an earlier `+k` and vice versa. The scanner tracks the running
// state rather than returning on the first `-k`, so a sequence like
// `set -k +k` is correctly seen as leaving keyword mode off.
func setMutatesAccountEnvironment(words []*syntax.Word) bool {
	keywordMode := false
	for idx := 0; idx < len(words); idx++ {
		value, literal := literalShellWord(words[idx])
		if !literal {
			// An operand this parser cannot evaluate could expand to -k.
			return true
		}
		// `--` and the first non-option operand both end option parsing: every
		// word after one is a positional parameter, so `set -- -k` assigns the
		// string "-k" to $1 and enables nothing. A lone `-` is also a bash
		// option terminator ("assign any remaining arguments to the positional
		// parameters"); `set +e - -k` assigns "-k" to $1 and does NOT enable
		// keyword mode. A `+` prefix is a turn-OFF flag in bash, not a
		// non-option operand, so it does NOT end the scan: `set +e -k` still
		// enables keyword mode and must be caught by the loop below.
		if value == "--" || value == "-" || (!strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "+")) {
			return keywordMode
		}
		// A long-form switch names its mode in the next word.
		//
		// `-o` has conditional arity: it consumes the following word as a mode
		// name ONLY when that word does not start with `-` or `+`. A real mode
		// name (pipefail, noclobber, keyword, …) never starts with either; a
		// word that does start with one is another option that the scan must
		// continue examining. When the next word is another option, `-o` behaves
		// as bare `-o` (prints current settings) and the shell processes the
		// following option normally — so `set +e -o -k` does enable keyword mode
		// via the `-k` that the `-o` branch must NOT swallow.
		if value == "-o" || value == "+o" {
			if idx+1 >= len(words) {
				// A bare `set -o` prints the current settings.
				continue
			}
			mode, ok := literalShellWord(words[idx+1])
			if !ok {
				return true
			}
			// Only treat the next word as the mode name when it cannot itself
			// be an option token. Mode names (pipefail, noclobber, …) never
			// start with `-` or `+`; a word that does start with one is an
			// option that must be examined on the next iteration.
			if strings.HasPrefix(mode, "-") || strings.HasPrefix(mode, "+") {
				continue
			}
			if mode == "keyword" {
				keywordMode = value == "-o"
			}
			idx++
			continue
		}
		// Short options cluster, so a guard matching only a lone "-k" walks
		// straight past "-ek" (the #3402 lesson). Track the running state
		// rather than returning immediately, so a later `+k` can cancel an
		// earlier `-k` (bash processes options left to right and the last
		// setting wins: `set -k +k` leaves keyword mode off).
		//
		// When a minus-prefixed cluster contains `o`, it has the same
		// conditional arity as the standalone `-o`: if the following word does
		// not start with `-` or `+`, that word is the mode name (and is consumed
		// by advancing idx). A plus-prefixed cluster containing `o` (`+eo`)
		// behaves as `+o` and turns the named mode OFF.
		prefix := value[0]
		tail := value[1:]
		if strings.ContainsRune(tail, 'o') {
			if idx+1 >= len(words) {
				// No following word: bare cluster with `o`, prints settings.
			} else {
				mode, ok := literalShellWord(words[idx+1])
				if !ok {
					return true
				}
				if !strings.HasPrefix(mode, "-") && !strings.HasPrefix(mode, "+") {
					// The following word is a mode name; consume it.
					if mode == "keyword" {
						keywordMode = prefix == '-'
					}
					idx++
					// Fall through to the `k` check: the cluster may contain `k`
					// in addition to `o` (e.g. `-ko pipefail`), and bash applies
					// all cluster characters — those before `o` and those after `o`
					// when the consumed name is valid. Skipping the check here
					// would miss a `k` in the same cluster.
				}
				// The following word is another option (or we just consumed the
				// mode name); fall through to the `k` check below.
			}
		}
		if strings.ContainsRune(tail, 'k') {
			keywordMode = prefix == '-'
		}
	}
	return keywordMode
}

// hashMutatesAccountEnvironment reports whether a `hash` call remaps a command
// name to a path of its own choosing.
//
// `hash -p pathname name` makes `name` resolve to `pathname`, so every later
// executable-name check in this walk answers about a different binary than the
// one that will actually run: after `hash -p /usr/bin/env runner`, the
// otherwise-unknown `runner` IS env, and `runner CODEX_HOME=/other codex`
// applies the replacement root through a name this walk never modelled.
//
// Only the remapping form is refused — `hash -r` and `hash name` merely
// maintain the lookup cache and leave every name meaning what it meant.
func hashMutatesAccountEnvironment(words []*syntax.Word) bool {
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if value == "--" || !strings.HasPrefix(value, "-") {
			return false
		}
		if strings.ContainsRune(value[1:], 'p') {
			return true
		}
	}
	return false
}

func isAccountDeclarationBuiltin(word *syntax.Word) bool {
	for _, name := range []string{"export", "readonly", "declare", "typeset", "local"} {
		if isBareName(word, name) {
			return true
		}
	}
	return false
}

func declarationMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for _, word := range words {
		if name, assignment := shellWordAssignmentName(word); assignment {
			if accountEnvironmentOperandDenied(name, names) {
				return true
			}
			options = false
			continue
		}
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			if value == "--" {
				options = false
				continue
			}
			if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
				if value == "-n" || value == "+n" || value == "--nameref" || value == "-i" {
					return true
				}
				if len(value) != 2 {
					return true
				}
				continue
			}
			options = false
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func readMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for len(words) > 0 && wordEquals(words[0], "-r") {
		words = words[1:]
	}
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if strings.HasPrefix(value, "-") {
			return true
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func getoptsMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	if len(words) < 2 {
		return false
	}
	name, literal := literalShellWord(words[1])
	if !literal {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func printfMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	if len(words) == 0 {
		return false
	}
	option, literal := literalShellWord(words[0])
	if !literal {
		return true
	}
	if strings.HasPrefix(option, "-v") && option != "-v" {
		return true
	}
	if option != "-v" || len(words) < 2 {
		return false
	}
	name, literal := literalShellWord(words[1])
	if !literal {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func accountEnvironmentOperandDenied(value string, names map[string]struct{}) bool {
	name := value
	if strings.IndexByte(name, '[') >= 0 {
		// Bash evaluates indexed-array subscripts as arithmetic and can assign a
		// protected variable while resolving an unrelated base name. Proving the
		// subscript is side-effect-free requires runtime semantics, so fail closed.
		return true
	}
	if equal := strings.IndexByte(name, '='); equal >= 0 {
		name = name[:equal]
	}
	return accountEnvironmentNameDenied(name, names)
}
