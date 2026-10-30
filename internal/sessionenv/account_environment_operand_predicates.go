package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

func accountEnvironmentNameDenied(name string, names map[string]struct{}) bool {
	_, denied := names[name]
	return denied
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

// envCallMutatesAccountEnvironment reports whether the env invocation formed by
// words mutates a denied name or execs something unprovable. requireCommand
// restricts that verdict to env invocations carrying a command word: print-mode
// env mutates only its own process, which is the right reading when the words
// sit in an unrecognized wrapper's tail rather than forming the command itself —
// an `env CODEX_HOME=/tmp` with nothing after it runs env's print path, not an
// override of a running agent.
//
// It is envcommand.Parse over the words literalized by envArgvWord — any word
// that fails envArgvWord refuses the call, wherever it sits — computed from
// per-position memos so that nested env words cost O(1) each (#4966).
func envCallMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	key := envCallKey{word: words[0], requireCommand: requireCommand}
	if answer, seen := memo.envCalls[key]; seen {
		return answer
	}
	answer := envCallMutatesAccountEnvironmentUncached(words, names, requireCommand, memo)
	memo.envCalls[key] = answer
	return answer
}

func envCallMutatesAccountEnvironmentUncached(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if memo.envArgvSuffixUnprovable(words) {
		return true
	}
	scan := memo.envScan(words, envcommand.Start, names)
	if scan.refused {
		return true
	}
	if requireCommand && scan.command == nil {
		return false
	}
	if scan.denied {
		return true
	}
	if scan.command == nil {
		return false
	}
	// env's command word is literal: envArgvWord passes a non-literal word
	// only when it spells an assignment, which the scan consumed as one. So
	// the operand-tail judgment — unwrap, then judge the peeled command — is
	// exactly the command's verdict, and shares its memo. The walk here has
	// no access to tainted; the taint check is applied at the arithmetic-node
	// level by nodeMutatesAccountEnvironment.
	return wrapperOperandTailMutates(scan.command, names, memo)
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
	command, literal := literalShellWord(words[0])
	// A sibling shell may read profiles, stdin, a script, or a command string.
	// The only statically proven form is the same absolute, startup-free command
	// AccountShellCommand generates for a dedicated shell tab. What stdin can
	// carry is a property of the whole command, not of these words, so
	// ValidateAccountEnvironmentCommand checks it separately
	// (commandFeedsProvenShell).
	if !literal || !filepath.IsAbs(command) {
		return false
	}
	// zsh's startup-freedom is only half in its argv: the other half is the
	// ZDOTDIR pin, which ApplyAccountEnvironment attaches to the exact
	// generated command alone. Inside a longer command, zsh runs with whatever
	// environment a wrapper or earlier statement hands it, so it is never
	// proven here; ValidateAccountEnvironmentCommand admits the exact form
	// before this walk runs (#4474 review).
	if filepath.Base(command) == "zsh" {
		return false
	}
	// Every rejection that needs only the head and the word count runs before
	// the arguments are literalized: fileHasProvenShell asks this of every
	// suffix of every call, so literalizing first made any long command
	// quadratic (#4966). A suffix is literalized only when its length already
	// matches the trusted form's, which is a handful of words.
	want := trustedAccountShellArgs(command)
	if want == nil || len(words)-1 != len(want) {
		return false
	}
	args, literal := literalCommandArgs(words[1:])
	return literal && slices.Equal(args, want)
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
	return accountEnvironmentOperandDeniedWith(value, names, true)
}

// accountEnvironmentFreeTextOptionValueDenied judges the value of an attached
// `--opt=value` on a wrapper tail this walk does not model. Such a value is
// free text unless it names a denied variable or carries a denied assignment,
// so the subscript rule applies only where the value is SHAPED like a variable
// operand: an identifier immediately followed by `[`. A bracket anywhere else
// is a glob or character class (`--ignore=[a-c]*`), which is no evidence of a
// mutation — and the same text as a separate word is never judged at all.
func accountEnvironmentFreeTextOptionValueDenied(value string, names map[string]struct{}) bool {
	bracket := strings.IndexByte(value, '[')
	subscripted := bracket > 0 && isShellIdentifier(value[:bracket])
	return accountEnvironmentOperandDeniedWith(value, names, subscripted || bracket < 0)
}

func isShellIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// accountEnvironmentOperandDeniedWith is the shared rule; subscriptRule false
// skips only the blanket bracket refusal, never the name/assignment check.
func accountEnvironmentOperandDeniedWith(value string, names map[string]struct{}, subscriptRule bool) bool {
	name := value
	if subscriptRule && strings.IndexByte(name, '[') >= 0 {
		// Bash evaluates indexed-array subscripts as arithmetic and can assign a
		// protected variable while resolving an unrelated base name. Proving the
		// subscript is side-effect-free requires runtime semantics, so fail closed.
		return true
	}
	if equal := strings.IndexByte(name, '='); equal >= 0 {
		// Cut the operand at the first `=` to reach its assignment target. bash's
		// `NAME+=value` append operator leaves a trailing `+` on the target once
		// the cut lands (`CODEX_HOME+=/other` -> `CODEX_HOME+`), so a guarded name
		// then misses the denied-set match and an append-assignment mutates the
		// very variable this predicate exists to protect. Strip the trailing `+`
		// so the append form is judged by its real target. The bare top-level
		// form is caught earlier by the parser's `*syntax.Assign` (Append) node;
		// this is the path that reaches a wrapper-peeled word list.
		name = name[:equal]
		name = strings.TrimSuffix(name, "+")
	}
	return accountEnvironmentNameDenied(name, names)
}
