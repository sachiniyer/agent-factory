package tmuxguard

import (
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// validateEnvPrefix recognizes a closed set of GNU env options. Split-string
// is intentionally unsupported because it performs a second round of command
// construction; unknown spellings and future options fail closed.
func validateEnvPrefix(words []string) error {
	if _, err := envcommand.Parse(words, envcommand.Policy{}); err != nil {
		return errUnsupportedShell
	}
	return nil
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
