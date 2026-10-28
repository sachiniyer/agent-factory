// Package envcommand parses the closed subset of GNU env that Agent Factory
// can reason about without executing it. Receipt routing and session-env
// command parsing use this package so option consumption cannot drift between
// them.
package envcommand

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported marks an env invocation whose effects cannot be established
// statically. Callers must fail closed rather than guessing past this error.
var ErrUnsupported = errors.New("unsupported env invocation")

// Policy controls whether Parse accepts environment assignments. Assignments
// can change command resolution, so callers that must model literal ones opt
// in; all current callers pass AllowAssignments: true.
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
	state := Start
	for idx := 0; idx < len(args); {
		operand := func() (string, bool) {
			if idx+1 >= len(args) {
				return "", false
			}
			return args[idx+1], true
		}
		step, err := Advance(args[idx], operand, state, policy)
		if err != nil {
			return result, err
		}
		if step.Clear {
			result.ClearEnvironment = true
			result.Mutations = nil
		}
		if step.HasMutation {
			result.Mutations = append(result.Mutations, step.Mutation)
		}
		if step.HasChdir {
			result.Chdir = step.Chdir
		}
		if step.Command {
			result.CommandIndex = idx
			return result, nil
		}
		idx += step.Width
		state = step.Next
	}
	return result, nil
}

// State is the part of Parse's scan that carries from one argument to the
// next: whether options are still recognized, and whether an assignment has
// been seen. Every scan begins at Start.
type State struct {
	options     bool
	assignments bool
}

// Start is the State Parse begins in.
var Start = State{options: true}

// Step is the effect of the argument at one position, as Parse applies it.
// Command marks the position as env's command; otherwise the scan continues
// Width arguments later in state Next.
type Step struct {
	Width       int
	Next        State
	Clear       bool
	Mutation    Mutation
	HasMutation bool
	Chdir       string
	HasChdir    bool
	Command     bool
}

// Advance applies Parse's rule to one argument. operand returns the argument
// after it, for the options that take one as a separate word; it is called
// only by those options, so a caller may literalize lazily. The Step depends
// only on (arg, operand, state, policy), which is what lets a caller that
// scans many suffixes of one argv memoize the scan per (position, state)
// instead of re-parsing every suffix (#4966).
func Advance(arg string, operand func() (string, bool), state State, policy Policy) (Step, error) {
	if state.options {
		switch {
		case arg == "--":
			return Step{Width: 1, Next: State{assignments: state.assignments}}, nil
		case arg == "-":
			return Step{Width: 1, Next: State{assignments: state.assignments}, Clear: true}, nil
		case arg == "-i" || arg == "--ignore-environment":
			return Step{Width: 1, Next: state, Clear: true}, nil
		case arg == "--help" || arg == "--version":
			return Step{}, unsupported(arg, "option exits env without running a command")
		case arg == "-0" || arg == "--null":
			return Step{}, unsupported(arg, "null output mode cannot run a command")
		case arg == "-v" || arg == "--debug" || arg == "--list-signal-handling":
			return Step{Width: 1, Next: state}, nil
		case arg == "-S" || strings.HasPrefix(arg, "-S") || arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
			return Step{}, unsupported(arg, "split-string constructs another command")
		case arg == "-u" || arg == "--unset":
			name, ok := operand()
			if !ok {
				return Step{}, unsupported(arg, "missing variable name")
			}
			if err := validateName(name); err != nil {
				return Step{}, err
			}
			return Step{Width: 2, Next: state, Mutation: Mutation{Name: name, Unset: true}, HasMutation: true}, nil
		case strings.HasPrefix(arg, "-u") && len(arg) > 2:
			name := strings.TrimPrefix(arg, "-u")
			if err := validateName(name); err != nil {
				return Step{}, err
			}
			return Step{Width: 1, Next: state, Mutation: Mutation{Name: name, Unset: true}, HasMutation: true}, nil
		case strings.HasPrefix(arg, "--unset="):
			name := strings.TrimPrefix(arg, "--unset=")
			if err := validateName(name); err != nil {
				return Step{}, err
			}
			return Step{Width: 1, Next: state, Mutation: Mutation{Name: name, Unset: true}, HasMutation: true}, nil
		case arg == "-C" || arg == "--chdir":
			dir, ok := operand()
			if !ok {
				return Step{}, unsupported(arg, "missing directory")
			}
			if err := validateNonEmptyLiteral(dir, "chdir"); err != nil {
				return Step{}, err
			}
			return Step{Width: 2, Next: state, Chdir: dir, HasChdir: true}, nil
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			dir := strings.TrimPrefix(arg, "-C")
			if err := validateNonEmptyLiteral(dir, "chdir"); err != nil {
				return Step{}, err
			}
			return Step{Width: 1, Next: state, Chdir: dir, HasChdir: true}, nil
		case strings.HasPrefix(arg, "--chdir="):
			dir := strings.TrimPrefix(arg, "--chdir=")
			if err := validateNonEmptyLiteral(dir, "chdir"); err != nil {
				return Step{}, err
			}
			return Step{Width: 1, Next: state, Chdir: dir, HasChdir: true}, nil
		case signalOption(arg):
			return Step{Width: 1, Next: state}, nil
		case strings.HasPrefix(arg, "-"):
			return Step{}, unsupported(arg, "unknown option")
		}
	}

	name, value, assignment := SplitAssignment(arg)
	if assignment {
		if !policy.AllowAssignments {
			return Step{}, unsupported(arg, "environment assignments are not allowed by this policy")
		}
		if err := validateLiteral(value, name); err != nil {
			return Step{}, err
		}
		return Step{Width: 1, Next: State{assignments: true}, Mutation: Mutation{Name: name, Value: value}, HasMutation: true}, nil
	}
	if state.assignments && strings.HasPrefix(arg, "-") {
		return Step{}, unsupported(arg, "option-like argument after an assignment")
	}
	return Step{Command: true}, nil
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
