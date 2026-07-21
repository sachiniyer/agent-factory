package tmuxguard

import (
	"fmt"
	"strings"
	"unicode"
)

// envTarget recognizes a closed set of GNU env options. Split-string is
// intentionally unsupported because it performs a second round of command
// construction; unknown spellings and future options fail closed. Assignments
// are preserved for the selected program policy.
type envEffects struct {
	assigned bool
	chdir    bool
}

func envTarget(args []shellWord) (target []shellWord, noCommand bool, effects envEffects, err error) {
	i := 0
	optionsDone := false
	for i < len(args) && !optionsDone {
		if !args[i].resolved {
			return nil, false, effects, errUnsupportedShell
		}
		arg := args[i].literal
		switch {
		case arg == "--":
			optionsDone = true
			i++
		case isAssignment(arg) || !strings.HasPrefix(arg, "-"):
			optionsDone = true
		case arg == "-" || arg == "-i" || arg == "-0" || arg == "-v" ||
			arg == "--debug" || arg == "--ignore-environment" || arg == "--null":
			i++
		case arg == "--help" || arg == "--list-signal-handling" || arg == "--version":
			if len(args) != i+1 {
				return nil, false, effects, errUnsupportedShell
			}
			return nil, true, effects, nil
		case arg == "-S" || strings.HasPrefix(arg, "-S") || arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
			return nil, false, effects, errUnsupportedShell
		case arg == "-u" || arg == "-C" || arg == "--unset" || arg == "--chdir":
			if i+1 >= len(args) || !args[i+1].resolved || args[i+1].literal == "" {
				return nil, false, effects, errUnsupportedShell
			}
			effects.chdir = effects.chdir || arg == "-C" || arg == "--chdir"
			i += 2
		case (strings.HasPrefix(arg, "-u") || strings.HasPrefix(arg, "-C")) && len(arg) > 2:
			effects.chdir = effects.chdir || strings.HasPrefix(arg, "-C")
			i++
		case strings.HasPrefix(arg, "--unset=") || strings.HasPrefix(arg, "--chdir="):
			effects.chdir = effects.chdir || strings.HasPrefix(arg, "--chdir=")
			i++
		case signalEnvOption(arg):
			i++
		default:
			return nil, false, effects, fmt.Errorf("%w: unknown env option", errUnsupportedShell)
		}
	}

	for i < len(args) {
		if !args[i].resolved {
			return nil, false, effects, errUnsupportedShell
		}
		arg := args[i].literal
		if !isAssignment(arg) {
			return args[i:], false, effects, nil
		}
		effects.assigned = true
		i++
	}
	return nil, true, effects, nil
}

func signalEnvOption(arg string) bool {
	for _, option := range []string{"--block-signal", "--default-signal", "--ignore-signal"} {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func shellCommandPayloadWords(args []shellWord) (string, bool, error) {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return "", false, errUnsupportedShell
		}
		arg := args[i].literal
		switch {
		case arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-"):
			return "", false, nil
		case arg == "-c":
			if i+1 >= len(args) || !args[i+1].resolved {
				return "", false, errUnsupportedShell
			}
			return args[i+1].literal, true, nil
		case arg == "-o" || arg == "-O":
			if i+1 >= len(args) || !args[i+1].resolved {
				return "", false, errUnsupportedShell
			}
			i++
		case strings.HasPrefix(arg, "--"):
			if !knownShellLongOption(arg) {
				return "", false, errUnsupportedShell
			}
		case strings.ContainsRune(arg[1:], 'c'):
			flags := arg[1:]
			if flags[len(flags)-1] != 'c' || !knownShellFlags(flags[:len(flags)-1]) ||
				i+1 >= len(args) || !args[i+1].resolved {
				return "", false, errUnsupportedShell
			}
			return args[i+1].literal, true, nil
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

// timeoutTarget recognizes the complete option prefix documented by GNU
// timeout. Options and their operands must be literal; once the duration has
// been consumed, the remaining words are a new command and are inspected in
// their real executable/argument positions.
func timeoutTarget(args []shellWord) (target []shellWord, noCommand bool, err error) {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return nil, false, errUnsupportedShell
		}
		arg := args[i].literal
		switch {
		case arg == "--help" || arg == "--version":
			if len(args) != 1 {
				return nil, false, errUnsupportedShell
			}
			return nil, true, nil
		case arg == "--foreground" || arg == "--preserve-status" || arg == "--verbose" || arg == "-v":
		case arg == "-k" || arg == "-s" || arg == "--kill-after" || arg == "--signal":
			if i+1 >= len(args) || !args[i+1].resolved || args[i+1].literal == "" {
				return nil, false, errUnsupportedShell
			}
			i++
		case (strings.HasPrefix(arg, "-k") || strings.HasPrefix(arg, "-s")) && len(arg) > 2:
		case strings.HasPrefix(arg, "--kill-after=") || strings.HasPrefix(arg, "--signal="):
		case arg == "--":
			if i+2 >= len(args) || !args[i+1].resolved || args[i+1].literal == "" {
				return nil, false, errUnsupportedShell
			}
			return args[i+2:], false, nil
		case strings.HasPrefix(arg, "-"):
			return nil, false, errUnsupportedShell
		default:
			if i+1 >= len(args) {
				return nil, false, errUnsupportedShell
			}
			return args[i+1:], false, nil
		}
	}
	return nil, false, errUnsupportedShell
}

func commandTarget(args []shellWord) (target []shellWord, noCommand bool, err error) {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return nil, false, errUnsupportedShell
		}
		arg := args[i].literal
		switch {
		case arg == "--":
			if i+1 == len(args) {
				return nil, true, nil
			}
			return args[i+1:], false, nil
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			flags := arg[1:]
			if strings.IndexFunc(flags, func(flag rune) bool {
				return !strings.ContainsRune("pVv", flag)
			}) != -1 {
				return nil, false, errUnsupportedShell
			}
			if strings.ContainsAny(flags, "Vv") {
				return nil, true, nil
			}
		default:
			return args[i:], false, nil
		}
	}
	return nil, true, nil
}

func gitCommitReadsMessageFromStdin(args []string) bool {
	commandAt, ok := gitSubcommandAt(args)
	if !ok || commandAt >= len(args) || args[commandAt] != "commit" {
		return false
	}
	args = args[commandAt+1:]
	for i, arg := range args {
		switch {
		case (arg == "-F" || arg == "--file") && i+1 < len(args) && args[i+1] == "-":
			return true
		case arg == "-F-" || arg == "--file=-":
			return true
		}
	}
	return false
}

func gitSubcommandAt(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return i + 1, i+1 < len(args)
		case arg == "-c" || arg == "-C" || arg == "--config-env" || arg == "--exec-path" ||
			arg == "--git-dir" || arg == "--namespace" || arg == "--super-prefix" || arg == "--work-tree":
			if i+1 >= len(args) || args[i+1] == "" {
				return 0, false
			}
			i++
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
		case strings.HasPrefix(arg, "--config-env=") || strings.HasPrefix(arg, "--exec-path=") ||
			strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--namespace=") ||
			strings.HasPrefix(arg, "--super-prefix=") || strings.HasPrefix(arg, "--work-tree="):
		case arg == "--bare" || arg == "--glob-pathspecs" || arg == "--icase-pathspecs" ||
			arg == "--literal-pathspecs" || arg == "--no-pager" || arg == "--no-replace-objects" ||
			arg == "--noglob-pathspecs" || arg == "--paginate":
		case strings.HasPrefix(arg, "-"):
			return 0, false
		default:
			return i, true
		}
	}
	return 0, false
}
