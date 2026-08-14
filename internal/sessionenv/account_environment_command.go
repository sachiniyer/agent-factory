package sessionenv

import (
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
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
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
	return mutates
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
		return ok && accountEnvironmentNameDenied(name, names)
	default:
		return false
	}
}

func arithmeticIncrementMutatesAccountEnvironment(expr *syntax.UnaryArithm, names map[string]struct{}) bool {
	if expr.Op != syntax.Inc && expr.Op != syntax.Dec {
		return false
	}
	name, ok := arithmeticAccountEnvironmentName(expr.X)
	return ok && accountEnvironmentNameDenied(name, names)
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
	if _, literal := literalShellWord(words[0]); !literal {
		// A dynamic command name can resolve to env or a same-shell builtin such
		// as unset/export, so its effect on the selected identity is unprovable.
		return true
	}
	switch {
	case isBareName(words[0], "env"):
		return envCallMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "unset"):
		return unsetMutatesAccountEnvironment(words[1:], names)
	case isAccountDeclarationBuiltin(words[0]):
		return declarationMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "read"):
		return readMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "getopts"):
		return getoptsMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "printf"):
		return printfMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "eval"), isBareName(words[0], "."),
		isBareName(words[0], "source"), isBareName(words[0], "trap"),
		isBareName(words[0], "alias"):
		// These builtins execute or schedule another shell program in the same
		// environment, or define commands that do. Proving their effects would
		// require a second parser pass with runtime expansion, so a scoped sibling
		// refuses them.
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
		default:
			return words, false
		}
	}
	return nil, false
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
	return false
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
			if _, denied := names[value]; denied {
				return true
			}
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
			if _, denied := names[name]; denied {
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
				if len(value) != 2 {
					return true
				}
				continue
			}
			options = false
		}
		if _, denied := names[value]; denied {
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
		if _, denied := names[value]; denied {
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
	_, denied := names[name]
	return denied
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
	_, denied := names[name]
	return denied
}
