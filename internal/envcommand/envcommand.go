// Package envcommand parses the closed subset of GNU env that Agent Factory
// can reason about without executing it. Both tmux safety policy and receipt
// routing use this package so option consumption cannot drift between them.
package envcommand

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported marks an env invocation whose effects cannot be established
// statically. Callers must fail closed rather than guessing past this error.
var ErrUnsupported = errors.New("unsupported env invocation")

// Policy describes the one intentional consumer difference: the tmux guard
// rejects every assignment because it cannot prove an assignment does not
// change command resolution, while receipt routing must model literal ones.
type Policy struct {
	AllowAssignments bool
}

// Mutation is one ordered environment change made by env before its command.
type Mutation struct {
	Name  string
	Value string
	Unset bool
}

// Invocation is the statically known effect of one env process. CommandIndex
// is relative to the argument slice passed to Parse, or -1 when no command was
// present. Chdir is empty when env does not change directory.
type Invocation struct {
	ClearEnvironment bool
	Chdir            string
	Mutations        []Mutation
	CommandIndex     int
}

// Parse recognizes a closed set of GNU env options. Split-string is rejected
// because it performs another round of command construction; unknown options,
// dynamic operands, missing operands, and options after assignments are also
// rejected. This deliberately prefers an actionable refusal over silently
// modeling a different process environment or cwd.
func Parse(args []string, policy Policy) (Invocation, error) {
	result := Invocation{CommandIndex: -1}
	options := true
	assignments := false
	for idx := 0; idx < len(args); {
		arg := args[idx]
		if options {
			switch {
			case arg == "--":
				options = false
				idx++
				continue
			case arg == "-" || arg == "-i" || arg == "--ignore-environment":
				result.ClearEnvironment = true
				result.Mutations = nil
				idx++
				continue
			case arg == "--help" || arg == "--version":
				return result, unsupported(arg, "option exits env without running a command")
			case arg == "-0" || arg == "--null":
				return result, unsupported(arg, "null output mode cannot run a command")
			case arg == "-v" || arg == "--debug" || arg == "--list-signal-handling":
				idx++
				continue
			case arg == "-S" || strings.HasPrefix(arg, "-S") || arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
				return result, unsupported(arg, "split-string constructs another command")
			case arg == "-u" || arg == "--unset":
				if idx+1 >= len(args) {
					return result, unsupported(arg, "missing variable name")
				}
				name := args[idx+1]
				if err := validateName(name); err != nil {
					return result, err
				}
				result.Mutations = append(result.Mutations, Mutation{Name: name, Unset: true})
				idx += 2
				continue
			case strings.HasPrefix(arg, "-u") && len(arg) > 2:
				name := strings.TrimPrefix(arg, "-u")
				if err := validateName(name); err != nil {
					return result, err
				}
				result.Mutations = append(result.Mutations, Mutation{Name: name, Unset: true})
				idx++
				continue
			case strings.HasPrefix(arg, "--unset="):
				name := strings.TrimPrefix(arg, "--unset=")
				if err := validateName(name); err != nil {
					return result, err
				}
				result.Mutations = append(result.Mutations, Mutation{Name: name, Unset: true})
				idx++
				continue
			case arg == "-C" || arg == "--chdir":
				if idx+1 >= len(args) {
					return result, unsupported(arg, "missing directory")
				}
				if err := validateNonEmptyLiteral(args[idx+1], "chdir"); err != nil {
					return result, err
				}
				result.Chdir = args[idx+1]
				idx += 2
				continue
			case strings.HasPrefix(arg, "-C") && len(arg) > 2:
				dir := strings.TrimPrefix(arg, "-C")
				if err := validateNonEmptyLiteral(dir, "chdir"); err != nil {
					return result, err
				}
				result.Chdir = dir
				idx++
				continue
			case strings.HasPrefix(arg, "--chdir="):
				dir := strings.TrimPrefix(arg, "--chdir=")
				if err := validateNonEmptyLiteral(dir, "chdir"); err != nil {
					return result, err
				}
				result.Chdir = dir
				idx++
				continue
			case signalOption(arg):
				idx++
				continue
			case strings.HasPrefix(arg, "-"):
				return result, unsupported(arg, "unknown option")
			}
		}

		name, value, assignment := SplitAssignment(arg)
		if assignment {
			if !policy.AllowAssignments {
				return result, unsupported(arg, "environment assignments are not allowed by this policy")
			}
			if err := validateLiteral(value, name); err != nil {
				return result, err
			}
			assignments = true
			options = false
			result.Mutations = append(result.Mutations, Mutation{Name: name, Value: value})
			idx++
			continue
		}
		if assignments && strings.HasPrefix(arg, "-") {
			return result, unsupported(arg, "option-like argument after an assignment")
		}
		result.CommandIndex = idx
		return result, nil
	}
	return result, nil
}

// SplitAssignment recognizes an env NAME=VALUE operand. GNU env accepts names
// beyond shell identifiers; only empty names and embedded '=' are impossible.
func SplitAssignment(arg string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(arg, "=")
	return name, value, ok && name != ""
}

// IsLiteral is conservative because the launch tokenizer removes quote
// provenance. Rejecting a quoted literal '$' is preferable to interpreting an
// expansion as a path and polling the wrong receipt store.
func IsLiteral(value string) bool {
	return !strings.ContainsAny(value, "$`;&|<>\n\r") && !strings.HasPrefix(value, "~")
}

func validateName(name string) error {
	if name == "" || strings.ContainsRune(name, '=') || !IsLiteral(name) {
		return unsupported(name, "invalid or dynamic variable name")
	}
	return nil
}

func validateLiteral(value, field string) error {
	if !IsLiteral(value) {
		return unsupported(value, fmt.Sprintf("%s uses shell expansion or control syntax; use a literal value", field))
	}
	return nil
}

func validateNonEmptyLiteral(value, field string) error {
	if value == "" {
		return unsupported(value, fmt.Sprintf("%s must not be empty", field))
	}
	return validateLiteral(value, field)
}

func signalOption(arg string) bool {
	for _, option := range []string{"--block-signal", "--default-signal", "--ignore-signal"} {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func unsupported(token, reason string) error {
	return fmt.Errorf("%w %q: %s", ErrUnsupported, token, reason)
}
