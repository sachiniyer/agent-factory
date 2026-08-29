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
			if nodeMutatesAccountEnvironment(node, names) {
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

func nodeMutatesAccountEnvironment(node syntax.Node, names map[string]struct{}) bool {
	switch node := node.(type) {
	case *syntax.CallExpr:
		return callMutatesAccountEnvironment(node, names)
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
		name, ok := arithmeticAccountEnvironmentName(expr.X)
		return ok && accountEnvironmentOperandDenied(name, names)
	default:
		return false
	}
}

func arithmeticIncrementMutatesAccountEnvironment(expr *syntax.UnaryArithm, names map[string]struct{}) bool {
	if expr.Op != syntax.Inc && expr.Op != syntax.Dec {
		return false
	}
	name, ok := arithmeticAccountEnvironmentName(expr.X)
	return ok && accountEnvironmentOperandDenied(name, names)
}

func arithmeticAccountEnvironmentName(expr syntax.ArithmExpr) (string, bool) {
	word, ok := expr.(*syntax.Word)
	if !ok {
		return "", false
	}
	return literalShellWord(word)
}

func callMutatesAccountEnvironment(call *syntax.CallExpr, names map[string]struct{}) bool {
	for _, assign := range call.Assigns {
		if assign != nil && assign.Name != nil {
			if _, denied := names[assign.Name.Value]; denied {
				return true
			}
		}
	}

	words, unsafe := unwrapAccountCommand(call.Args, names)
	if unsafe || len(words) == 0 {
		return unsafe
	}
	return unwrappedAccountCommandMutates(words, names)
}

func unwrappedAccountCommandMutates(words []*syntax.Word, names map[string]struct{}) bool {
	if _, literal := literalShellWord(words[0]); !literal {
		// A dynamic command name can resolve to env or a same-shell builtin such
		// as unset/export, so its effect on the selected identity is unprovable.
		return true
	}
	switch {
	case isAccountCommandName(words[0], "env"):
		return envCallMutatesAccountEnvironment(words[1:], names)
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
	case shellCommandIsUnproven(words):
		return true
	default:
		return false
	}
}

// unwrapAccountCommand removes shell wrappers that still execute the remaining
// words in the current environment. Dynamic or unsupported wrapper forms are
// unsafe because they could resolve to env or an environment-mutating builtin.
func unwrapAccountCommand(words []*syntax.Word, names map[string]struct{}) ([]*syntax.Word, bool) {
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
		default:
			return words, false
		}
	}
	return nil, false
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

func envCallMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	literals := make([]string, 0, len(words))
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			name, assignment := shellWordAssignmentName(word)
			if !assignment {
				return true
			}
			value = name + "=AF_DYNAMIC_VALUE"
		}
		literals = append(literals, value)
	}
	invocation, err := envcommand.Parse(literals, envcommand.Policy{AllowAssignments: true})
	if err != nil || invocation.ClearEnvironment {
		return true
	}
	for _, mutation := range invocation.Mutations {
		if _, denied := names[mutation.Name]; denied {
			return true
		}
	}
	if invocation.CommandIndex >= 0 {
		commandWords, unsafe := unwrapAccountCommand(words[invocation.CommandIndex:], names)
		if unsafe {
			return true
		}
		if len(commandWords) == 0 {
			return false
		}
		return unwrappedAccountCommandMutates(commandWords, names)
	}
	return false
}

func shellCommandIsUnproven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	if !literal || !knownShellName(filepath.Base(command)) {
		return false
	}
	return !accountShellCommandWordsProven(words)
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
func setMutatesAccountEnvironment(words []*syntax.Word) bool {
	for idx := 0; idx < len(words); idx++ {
		value, literal := literalShellWord(words[idx])
		if !literal {
			// An operand this parser cannot evaluate could expand to -k.
			return true
		}
		// `--` and the first non-option operand both end option parsing: every
		// word after one is a positional parameter, so `set -- -k` assigns the
		// string "-k" to $1 and enables nothing.
		if value == "--" || !strings.HasPrefix(value, "-") {
			return false
		}
		// A long-form switch names its mode in the next word. `+o keyword` turns
		// the mode OFF, so only the minus form is a switch on.
		if value == "-o" || value == "+o" {
			if idx+1 >= len(words) {
				// A bare `set -o` prints the current settings.
				continue
			}
			mode, ok := literalShellWord(words[idx+1])
			if !ok {
				return true
			}
			if value == "-o" && mode == "keyword" {
				return true
			}
			idx++
			continue
		}
		// Short options cluster, so a guard matching only a lone "-k" walks
		// straight past "-ek" (the #3402 lesson). "+k" DISABLES keyword mode and
		// is not a prefix match here.
		if strings.ContainsRune(value[1:], 'k') {
			return true
		}
	}
	return false
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
