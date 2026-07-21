package tmuxguard

import (
	"fmt"
	"strings"
	"unicode"
)

// validateEnvPrefix recognizes a closed set of GNU env options. Split-string
// is intentionally unsupported because it performs a second round of command
// construction; unknown spellings and future options fail closed.
func validateEnvPrefix(words []string) error {
	for len(words) > 0 {
		arg := words[0]
		switch {
		case arg == "--":
			return nil
		case isAssignment(arg):
			return errUnsupportedShell
		case arg == "-" || arg == "-i" || arg == "-0" || arg == "-v":
			words = words[1:]
		case arg == "--ignore-environment" || arg == "--null" || arg == "--debug":
			words = words[1:]
		case arg == "--list-signal-handling" || arg == "--help" || arg == "--version":
			words = words[1:]
		case arg == "-S" || strings.HasPrefix(arg, "-S") || arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
			return errUnsupportedShell
		case arg == "-u" || arg == "-C":
			if len(words) < 2 {
				return errUnsupportedShell
			}
			words = words[2:]
		case strings.HasPrefix(arg, "-u") || strings.HasPrefix(arg, "-C"):
			words = words[1:]
		case arg == "--unset" || arg == "--chdir":
			if len(words) < 2 {
				return errUnsupportedShell
			}
			words = words[2:]
		case strings.HasPrefix(arg, "--unset=") || strings.HasPrefix(arg, "--chdir="):
			words = words[1:]
		case signalEnvOption(arg):
			words = words[1:]
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("%w: unknown env option", errUnsupportedShell)
		default:
			return nil
		}
	}
	return nil
}

func signalEnvOption(arg string) bool {
	for _, option := range []string{"--block-signal", "--default-signal", "--ignore-signal"} {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func shellCommandPayload(args []string) (string, bool, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-"):
			return "", false, nil
		case arg == "-c":
			if i+1 >= len(args) {
				return "", false, errUnsupportedShell
			}
			return args[i+1], true, nil
		case arg == "-o" || arg == "-O":
			if i+1 >= len(args) {
				return "", false, errUnsupportedShell
			}
			i++
		case strings.HasPrefix(arg, "--"):
			if !knownShellLongOption(arg) {
				return "", false, errUnsupportedShell
			}
		case strings.ContainsRune(arg[1:], 'c'):
			flags := arg[1:]
			if flags[len(flags)-1] != 'c' || !knownShellFlags(flags[:len(flags)-1]) || i+1 >= len(args) {
				return "", false, errUnsupportedShell
			}
			return args[i+1], true, nil
		case !knownShellFlags(arg[1:]):
			return "", false, errUnsupportedShell
		}
	}
	return "", false, nil
}

func knownShellLongOption(arg string) bool {
	switch arg {
	case "--noediting", "--noprofile", "--norc", "--posix", "--restricted", "--verbose":
		return true
	default:
		return false
	}
}

func knownShellFlags(flags string) bool {
	return strings.IndexFunc(flags, func(flag rune) bool {
		return !strings.ContainsRune("abefhkmnprstuvxBCEHPT", flag)
	}) == -1
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range word[:eq] {
		if (i == 0 && !unicode.IsLetter(r) && r != '_') || (i > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_') {
			return false
		}
	}
	return true
}
