package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// isLastBackgroundPidWord reports whether a word is exactly `$!`, bare or
// double-quoted. The shell owns that parameter — it is not assignable — so it
// always expands to a decimal pid and can never become an option word.
func isLastBackgroundPidWord(word *syntax.Word) bool {
	if word == nil || len(word.Parts) != 1 {
		return false
	}
	part := word.Parts[0]
	if quoted, ok := part.(*syntax.DblQuoted); ok {
		if len(quoted.Parts) != 1 {
			return false
		}
		part = quoted.Parts[0]
	}
	exp, ok := part.(*syntax.ParamExp)
	return ok && exp.Param != nil && exp.Param.Value == "!" &&
		exp.Exp == nil && exp.Index == nil && exp.Slice == nil && exp.Repl == nil &&
		!exp.Length && !exp.Width && !exp.Excl && exp.Names == 0
}

func waitMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			// `$!` is the ONE expansion that cannot turn into an option: the shell
			// sets it to the last background pid and it is not assignable, so it is
			// always a job spec and ends option parsing exactly like a literal
			// operand does. That keeps `wait -p PID $!` working.
			if isLastBackgroundPidWord(words[0]) {
				return false
			}
			// Every other dynamic word is unsafe while option parsing is still
			// open — and for wait it is still open after `-p target`. This used to
			// concede that a dynamic word following a result target was "the
			// customary expanded job spec", but bash keeps reading options there:
			// with x=-p, `wait -p safe "$x" CODEX_HOME $!` expands to a SECOND -p,
			// retargets at CODEX_HOME, assigns it the job id and drops its export
			// attribute, so the child inherits no selected root at all.
			return true
		}
		if option == "--" || option == "-" || !strings.HasPrefix(option, "-") {
			return false
		}
		flags := option[1:]
		consumed := 1
		for idx, flag := range flags {
			switch flag {
			case 'f', 'n':
			case 'p':
				// Bash documents -p varname as a separate operand. Refuse
				// attached or dynamic spellings whose assignment target cannot
				// be proven, and keep scanning because repeated -p options use
				// the last target.
				if idx != len(flags)-1 || len(words) < 2 {
					return true
				}
				target, literal := literalShellWord(words[1])
				if !literal || accountEnvironmentOperandDenied(target, names) {
					return true
				}
				consumed = 2
			default:
				return true
			}
		}
		words = words[consumed:]
	}
	return false
}

func letMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for _, word := range words {
		expression, literal := literalShellWord(word)
		if !literal || accountSubscriptInArithmetic(expression, names) {
			return true
		}
		parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Arithmetic(strings.NewReader(expression))
		if err != nil {
			return true
		}
		mutates := false
		syntax.Walk(parsed, func(node syntax.Node) bool {
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

func accountSubscriptInArithmetic(expression string, names map[string]struct{}) bool {
	for name := range names {
		for offset := 0; offset < len(expression); {
			index := strings.Index(expression[offset:], name+"[")
			if index < 0 {
				break
			}
			index += offset
			if index == 0 || !isShellNameByte(expression[index-1]) {
				return true
			}
			offset = index + len(name)
		}
	}
	return false
}

func isShellNameByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func arrayReadMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for len(words) > 0 {
		value, literal := literalShellWord(words[0])
		if !literal {
			return true
		}
		words = words[1:]
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-t":
				continue
			case "-d":
				if len(words) == 0 {
					return true
				}
				if _, literal := literalShellWord(words[0]); !literal {
					return true
				}
				words = words[1:]
				continue
			}
			if strings.HasPrefix(value, "-") {
				// The remaining options accept arithmetic expressions or callbacks.
				// Either can assign an identity indirectly, so unsupported option
				// forms fail closed.
				return true
			}
		}
		if len(words) != 0 {
			return true
		}
		return accountEnvironmentOperandDenied(value, names)
	}
	return false
}
