package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type straceOptionResult uint8

const (
	straceOptionContinue straceOptionResult = iota
	straceOptionStops
	straceOptionUnsafe

	// These sets mirror the short-option arity in strace(1). Help and version
	// stop parsing separately because they never execute a child.
	straceShortOptionsNoValue   = "ACDcdfiknqrtTvwxyYzZ"
	straceShortOptionsWithValue = "abeEIoOpPsSuUX"
)

// unwrapStrace returns the command strace executes. Unlike an unclassified
// literal process, strace assigns executable meaning to one of its operands,
// so every word up to that operand must be understood before the child can be
// inspected. Unknown options and unreduced words fail closed; treating either
// as "not an option" would silently move the executable boundary.
func unwrapStrace(words []*syntax.Word, names map[string]struct{}) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			return words[1:], false
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			return words, false
		}

		var consumed int
		var result straceOptionResult
		if strings.HasPrefix(option, "--") {
			consumed, result = parseStraceLongOption(words, names)
		} else {
			consumed, result = parseStraceShortOptions(words, names)
		}
		switch result {
		case straceOptionContinue:
			words = words[consumed:]
		case straceOptionStops:
			return nil, false
		case straceOptionUnsafe:
			return nil, true
		}
	}
	return nil, false
}

func parseStraceLongOption(words []*syntax.Word, names map[string]struct{}) (int, straceOptionResult) {
	value, _ := literalShellWord(words[0])
	option, attachedValue, attached := strings.Cut(value, "=")
	switch option {
	case "--help", "--version":
		if attached {
			return 0, straceOptionUnsafe
		}
		return 0, straceOptionStops
	case "--env":
		operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
		if !ok || straceEnvironmentMutationUnsafe(operand, names) {
			return 0, straceOptionUnsafe
		}
		return consumed, straceOptionContinue
	case "--output":
		operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
		if !ok || straceOutputTargetUnsafe(operand) {
			return 0, straceOptionUnsafe
		}
		return consumed, straceOptionContinue
	case "--debug", "--follow-forks", "--instruction-pointer", "--kill-on-exit",
		"--no-abbrev", "--output-append-mode", "--output-separately", "--seccomp-bpf",
		"--successful-only", "--failed-only", "--summary", "--summary-only",
		"--summary-wall-clock", "--syscall-number":
		if attached {
			return 0, straceOptionUnsafe
		}
		return 1, straceOptionContinue
	case "--absolute-timestamps", "--daemonize", "--decode-fds", "--quiet",
		"--relative-timestamps", "--stack-trace", "--strings-in-hex", "--syscall-times", "--tips":
		// These options take an optional value only in attached `=value` form.
		return 1, straceOptionContinue
	case "--abbrev", "--argv0", "--attach", "--columns", "--const-print-style",
		"--decode-pids", "--detach-on", "--fault", "--inject", "--interruptible",
		"--kvm", "--raw", "--read", "--signal", "--stack-trace-frame-limit",
		"--status", "--string-limit", "--summary-columns", "--summary-sort-by",
		"--summary-syscall-overhead", "--syscall-limit", "--trace", "--trace-fds",
		"--trace-path", "--user", "--verbose", "--write":
		_, consumed, ok := straceOptionValue(words, attachedValue, attached)
		if !ok {
			return 0, straceOptionUnsafe
		}
		return consumed, straceOptionContinue
	default:
		return 0, straceOptionUnsafe
	}
}

func parseStraceShortOptions(words []*syntax.Word, names map[string]struct{}) (int, straceOptionResult) {
	value, _ := literalShellWord(words[0])
	for idx := 1; idx < len(value); idx++ {
		flag := value[idx]
		switch {
		case flag == 'h' || flag == 'V':
			return 0, straceOptionStops
		case strings.ContainsRune(straceShortOptionsNoValue, rune(flag)):
			continue
		case strings.ContainsRune(straceShortOptionsWithValue, rune(flag)):
			attached := idx+1 < len(value)
			attachedValue := ""
			if attached {
				attachedValue = value[idx+1:]
			}
			operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
			if !ok {
				return 0, straceOptionUnsafe
			}
			if flag == 'E' && straceEnvironmentMutationUnsafe(operand, names) {
				return 0, straceOptionUnsafe
			}
			if flag == 'o' && straceOutputTargetUnsafe(operand) {
				return 0, straceOptionUnsafe
			}
			return consumed, straceOptionContinue
		default:
			return 0, straceOptionUnsafe
		}
	}
	return 1, straceOptionContinue
}

func straceOptionValue(words []*syntax.Word, attachedValue string, attached bool) (string, int, bool) {
	if attached {
		return attachedValue, 1, attachedValue != ""
	}
	if len(words) < 2 {
		return "", 0, false
	}
	value, literal := literalShellWord(words[1])
	return value, 2, literal
}

func straceOutputTargetUnsafe(operand string) bool {
	return strings.HasPrefix(operand, "|") || strings.HasPrefix(operand, "!")
}

func straceEnvironmentMutationUnsafe(operand string, names map[string]struct{}) bool {
	name, _, _ := strings.Cut(operand, "=")
	if !validName(name) {
		return true
	}
	return accountEnvironmentNameDenied(name, names)
}
