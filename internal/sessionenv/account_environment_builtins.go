package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func unwrapNohup(words []*syntax.Word) ([]*syntax.Word, bool) {
	if len(words) > 0 && wordEquals(words[0], "--") {
		words = words[1:]
	}
	if len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal || strings.HasPrefix(option, "-") {
			return nil, true
		}
	}
	return words, false
}

func unwrapNice(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case option == "-n" || option == "--adjustment":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case strings.HasPrefix(option, "-n") || strings.HasPrefix(option, "--adjustment="):
			words = words[1:]
		case strings.HasPrefix(option, "-") && len(option) > 1:
			// Traditional nice accepts a bare numeric adjustment such as -10.
			if strings.Trim(option[1:], "0123456789") != "" {
				return nil, true
			}
			words = words[1:]
		default:
			return words, false
		}
	}
	return nil, false
}

func unwrapTimeout(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			words = words[1:]
			if len(words) < 2 {
				return nil, false
			}
			return words[1:], false
		case option == "-k" || option == "--kill-after" || option == "-s" || option == "--signal":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case option == "--foreground" || option == "--preserve-status" || option == "-v" || option == "--verbose":
			words = words[1:]
		case strings.HasPrefix(option, "--kill-after=") || strings.HasPrefix(option, "--signal="):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return nil, true
		default:
			if len(words) < 2 {
				return nil, false
			}
			return words[1:], false
		}
	}
	return nil, false
}

func unwrapSetsid(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch option {
		case "--":
			return words[1:], false
		case "-h", "--help", "-V", "--version":
			return nil, false
		case "-c", "--ctty", "-f", "--fork", "-w", "--wait":
			words = words[1:]
		default:
			if strings.HasPrefix(option, "-") && len(option) > 1 {
				for _, flag := range option[1:] {
					if flag != 'c' && flag != 'f' && flag != 'w' {
						return nil, true
					}
				}
				words = words[1:]
				continue
			}
			return words, false
		}
	}
	return nil, false
}

func unwrapStdbuf(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case option == "--help" || option == "--version":
			return nil, false
		case option == "-i" || option == "--input" ||
			option == "-o" || option == "--output" ||
			option == "-e" || option == "--error":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case strings.HasPrefix(option, "--input=") ||
			strings.HasPrefix(option, "--output=") ||
			strings.HasPrefix(option, "--error="):
			words = words[1:]
		case len(option) > 2 && option[0] == '-' && strings.ContainsRune("ioe", rune(option[1])):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return nil, true
		default:
			return words, false
		}
	}
	return nil, false
}

// unwrapIonice removes an `ionice` prefix so the command it schedules is what
// gets inspected. util-linux ionice runs an arbitrary COMMAND after its
// options exactly as nice does, and this repository already models it as an
// executable wrapper (session/tmux/resume.go). Left unwrapped it read as an
// opaque leaf program whose arguments were inert, so
// `ionice -c 3 sh -c 'unset CODEX_HOME; codex'` reached the default-safe
// return and the nested shell removed the selected root before launch.
func unwrapIonice(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case option == "-t" || option == "--ignore":
			words = words[1:]
		case option == "-c" || option == "--class" || option == "-n" || option == "--classdata":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			words = words[2:]
		case strings.HasPrefix(option, "-c") || strings.HasPrefix(option, "-n") ||
			strings.HasPrefix(option, "--class=") || strings.HasPrefix(option, "--classdata="):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			// -p/-P/-u retune an EXISTING process and run no command at all, so
			// there is nothing here to unwrap; every other form is unmodelled.
			return nil, true
		default:
			return words, false
		}
	}
	return nil, false
}

// unwrapTaskset is unwrapIonice for `taskset`, with one extra step: taskset's
// first OPERAND is the affinity mask (or, after -c, the cpu list), and the
// command it runs begins only after it.
func unwrapTaskset(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return tasksetCommandAfterMask(words[1:])
		case option == "-a" || option == "--all-tasks" ||
			option == "-c" || option == "--cpu-list":
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			// -p rebinds an EXISTING pid and runs no command; anything else is
			// unmodelled.
			return nil, true
		default:
			return tasksetCommandAfterMask(words)
		}
	}
	return nil, false
}

func tasksetCommandAfterMask(words []*syntax.Word) ([]*syntax.Word, bool) {
	if len(words) == 0 {
		return nil, false
	}
	if _, literal := literalShellWord(words[0]); !literal {
		return nil, true
	}
	return words[1:], false
}

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
