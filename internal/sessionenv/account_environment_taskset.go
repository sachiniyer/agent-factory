package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unwrapTaskset is unwrapIonice for `taskset`, with one extra step: taskset's
// first OPERAND is the affinity mask (or, after -c, the cpu list), and the
// command it runs begins only after it.
func unwrapTaskset(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("taskset")
	for len(words) > 0 {
		if cached, seen := run.visit(words, nil); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			// taskset's selector behaves as ionice's does: -p switches it to
			// operating on an existing PID, so no expansion of an attached quoted
			// value launches a child. Measured on util-linux 2.39.3, `taskset
			// -p"$P" /bin/echo X` reports `invalid PID argument` for an empty and a
			// valid $P alike, and `--pid="$P"` is rejected outright with `option
			// '--pid' doesn't allow an argument` — every spelling exits childless,
			// so the name is matched with any attached value cut away.
			prefix, quoted := literalPrefixBeforeSimpleQuotedParameter(words[0])
			if name, _, _ := strings.Cut(prefix, "="); quoted && tasksetProcessOnlyOption(name) {
				if shadowedOperandTailMutates(words[1:], names, memo) {
					return run.done(nil, true)
				}
				return run.done(words[1:], false)
			}
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			return run.done(tasksetCommandAfterMask(words[1:], names, memo))
		case utilLinuxTerminalOption(option, "acp"):
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case tasksetProcessOnlyOption(option):
			// -p switches taskset from command execution to inspecting or
			// updating an existing PID, so no child environment exists to
			// mutate on the real binary. The operand tail is still inspected:
			// the basename match cannot distinguish taskset from a
			// PATH-shadowed script that execs whatever follows the selector.
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case option == "-a" || option == "--all-tasks" ||
			option == "-c" || option == "--cpu-list" ||
			tasksetChildLaunchingLongAbbrev(option):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return run.done(nil, true)
		default:
			return run.done(tasksetCommandAfterMask(words, names, memo))
		}
	}
	return run.done(nil, false)
}

func tasksetProcessOnlyOption(option string) bool {
	// util-linux uses getopt_long, so every nonempty prefix of --pid is the
	// same process-only mode while that prefix is unambiguous. Accepting a
	// prefix unsupported by the installed taskset is harmless: taskset exits
	// before it could launch a child.
	if len(option) > 2 && strings.HasPrefix("--pid", option) {
		return true
	}
	if len(option) < 2 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for _, flag := range option[1:] {
		if flag == 'p' {
			return true
		}
		if flag != 'a' && flag != 'c' {
			return false
		}
	}
	return false
}

// tasksetChildLaunchingLongAbbrev recognizes the unambiguous getopt_long
// abbreviations of --all-tasks and --cpu-list, which both launch a masked
// child on util-linux (measured on 2.39.3: `taskset --all 0x1 /bin/echo X`
// and `taskset --cpu 0-3 /bin/echo X` both exec the child). The exact-equality
// arm already handles the spelled-out forms; this closes the abbreviation
// gap so the child tail is judged by tasksetCommandAfterMask rather than
// fail-closed at the option arm. The `len > 2` / `--` guard confines the check
// to long options, and --all-tasks / --cpu-list have no other --a* / --c*
// long-option neighbors in taskset (--help / --version are handled earlier by
// utilLinuxTerminalOption; --pid by tasksetProcessOnlyOption), so every
// matched prefix is unambiguous. This mirrors the existing
// strings.HasPrefix("--pid", option) idiom used throughout this file family.
func tasksetChildLaunchingLongAbbrev(option string) bool {
	if len(option) <= 2 || !strings.HasPrefix(option, "--") {
		return false
	}
	return strings.HasPrefix("--all-tasks", option) || strings.HasPrefix("--cpu-list", option)
}

func tasksetCommandAfterMask(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	if len(words) == 0 {
		return nil, false
	}
	// Same rule as ionice's class value, and for the same reason: the mask (or
	// cpu list) names CPUs, so only its ONE-WORD-ness matters, not its content.
	// Measured on util-linux 2.39.3, an empty or unparseable mask exits with
	// "failed to parse CPU mask"/"CPU list" before exec, and a valid one runs the
	// child — which the walk then inspects either way.
	if _, literal := literalShellWord(words[0]); !literal &&
		!isSimpleQuotedParameterWord(words[0]) {
		return nil, true
	}
	// The mask word stays a candidate like any other consumed operand: a
	// shadowed taskset need not skip it, so the mask-onward tail is judged as
	// a command before the real binary's child is returned.
	if wrapperOperandTailMutates(words, names, memo) {
		return nil, true
	}
	// The mask is judged above as a head; the real binary's child starts at
	// words[1:]. A shadowed `./taskset` can shift past the mask (and, after
	// -c, the cpu list) and exec any literal suffix of that child tail, so
	// every suffix is judged. Because these words ARE the real child's argv,
	// the child-tail scan drops the childless PID bound the selector and
	// terminal branches keep — a command may take any number of operands, so
	// the tail's length is not a mutation.
	if shadowedChildTailMutates(words[1:], names, memo) {
		return nil, true
	}
	return words[1:], false
}
