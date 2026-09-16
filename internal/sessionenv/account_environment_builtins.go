package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// operandTailMemo bounds the shadowed-wrapper operand walk. Every words slice
// inside one validation is a suffix of the call's Args — the parser allocates
// each Word once — so the first element's pointer names a distinct remaining
// suffix, and the answer to "does the operand-onward tail mutate" depends only
// on that suffix. Without it the same suffix is walked once by the operand
// check and again by the enclosing unwrap loop's continuation, so nested
// value-taking wrappers recurred exponentially (Codex on #4465: ~3s at depth
// 20 of `nice -n nice ...`, unbounded at 25).
type operandTailMemo map[*syntax.Word]bool

// wrapperOperandTailMutates keeps a consumed option operand a candidate for
// inspection. The modeled wrappers match by basename, which cannot prove the
// binary is real util-linux: a repository-local or PATH-shadowed `ionice`
// containing `shift; exec "$@"` parses `-c` differently and executes what the
// model discarded as a class operand.
//
// words begins at the operand. A literal operand is judged as the head of a
// command line (`-c env -u CODEX_HOME codex` hides `env -u CODEX_HOME codex`
// under a shadowed binary). A dynamic operand is provably one argv word — the
// caller's gate — but the expansion itself is the shadowed command's HEAD:
// it can resolve to `env`, a same-shell builtin such as unset/export, or a
// shell that reads the tail as a script — exactly the position
// unwrappedAccountCommandMutates refuses outright. Judging only the env
// expansion left `ionice -c "$CLASS" /tmp/launch-agent` accepted while the
// literal `-c sh /tmp/launch-agent` refused (Codex on #4465), so the dynamic
// case fails closed. The option consumption that follows still covers the
// real binary's reading.
func wrapperOperandTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	if answer, seen := memo[words[0]]; seen {
		return answer
	}
	answer := wrapperOperandTailMutatesUncached(words, names, memo)
	memo[words[0]] = answer
	return answer
}

func wrapperOperandTailMutatesUncached(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if _, literal := literalShellWord(words[0]); !literal {
		return true
	}
	tail, unsafe := unwrapAccountCommand(words, names, memo)
	if unsafe {
		return true
	}
	if len(tail) == 0 {
		return false
	}
	return unwrappedAccountCommandMutates(tail, names, nil, memo)
}

// shadowedTailOperandLimit bounds a childless tail. The real binaries take a
// handful of words there — PIDs and the odd permuted option; taskset takes one
// PID — and a saved command has no use for more: PIDs do not survive a
// restart, and `$(pgrep …)` is dynamic and already refused. The bound is per
// tail, so total work stays linear in the command's length.
const shadowedTailOperandLimit = 64

// shadowedOperandTailMutates fails closed when any word in a returned tail is
// not provably a single literal argv word — and when any literal boundary of
// that tail judges as a mutating command. It guards the childless tails —
// process-only selectors and terminal options — where the real util-linux
// binary consumes every remaining word as operand text (or never reaches
// them) and only the shadowed reading can execute one.
//
// Judging that tail from its first word alone let a literal operand mask what
// follows it: `./ionice -p"$PID" 123 "$CMD" /tmp/launch-agent` returned
// [123, "$CMD", ...] whose literal head read as an unrecognized command,
// while a repo-local ionice stripping a different operand count execs
// `sh /tmp/launch-agent` when CMD=sh (Codex on #4465). The all-literal case is
// the same hole with a named command: `./ionice -p 123 xargs
// --process-slot-var CODEX_HOME codex` is inert on the real binary, but a
// shadowed `shift 2; exec "$@"` lands on the xargs boundary and replaces the
// account root (Codex on #4465). A shadowed wrapper may discard ANY count of
// operands, so every literal suffix is a possible exec boundary and each is
// judged as one; the memoized wrapper walk keeps the scan polynomial.
//
// Each suffix judgment re-walks the rest of the tail, so the scan is quadratic
// in its length — 8000 literal PIDs took 8s against 6ms on master — and a tail
// past shadowedTailOperandLimit fails closed instead.
func shadowedOperandTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) > shadowedTailOperandLimit {
		return true
	}
	for i := range words {
		if wrapperOperandTailMutates(words[i:], names, memo) {
			return true
		}
	}
	return false
}

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

func unwrapNice(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
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
			if wrapperOperandTailMutates(words[1:], names, memo) {
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

func unwrapTimeout(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
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
			if _, literal := literalShellWord(words[0]); !literal &&
				!isSimpleQuotedParameterWord(words[0]) {
				return nil, true
			}
			if wrapperOperandTailMutates(words, names, memo) {
				return nil, true
			}
			return words[1:], false
		case option == "-k" || option == "--kill-after" || option == "-s" || option == "--signal":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
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
			// The duration is an operand like taskset's mask: it stays a
			// candidate for the shadowed reading.
			if wrapperOperandTailMutates(words, names, memo) {
				return nil, true
			}
			return words[1:], false
		}
	}
	return nil, false
}

func unwrapSetsid(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch option {
		case "--":
			return words[1:], false
		case "-h", "--help", "-V", "--version":
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
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

func unwrapStdbuf(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case option == "--help" || option == "--version":
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
		case option == "-i" || option == "--input" ||
			option == "-o" || option == "--output" ||
			option == "-e" || option == "--error":
			if len(words) < 2 {
				return nil, true
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return nil, true
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
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
func unwrapIonice(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	var scope ioniceProofScope
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			// An option token carrying a quoted value is still ONE argv word when
			// a literal '=' or literal value text pins its boundary, so the value
			// need not be literal for the token to be understood. Besides pinned
			// tokens, only process selectors and the unpinned shape the #4460
			// proof covers (ioniceQuotedOptionBoundaryPinned) are accepted here.
			prefix, quoted := literalPrefixBeforeSimpleQuotedParameter(words[0])
			if !quoted {
				return nil, true
			}
			// A process selector needs no boundary decision for its OWN operand:
			// it switches ionice to acting on already-running processes, so no
			// expansion of the attached value can launch a child. The words AFTER
			// the selector still return for inspection: isAccountCommandName
			// matches by basename, which cannot distinguish the real util-linux
			// binary from a PATH-shadowed or repo-local `ionice` that execs
			// whatever follows. On the real binary the tail is further PID
			// operands — measured on 2.39.3, `ionice -p"$PID" /bin/echo X` reports
			// `invalid PID argument` and prints nothing — so inspecting it as a
			// command refuses only what a shadowed wrapper could actually run.
			if ioniceProcessOnlyOption(prefix) {
				if !scope.admitExtension() || shadowedOperandTailMutates(words[1:], names, memo) {
					return nil, true
				}
				return words[1:], false
			}
			if !ioniceQuotedOptionBoundaryPinned(prefix) {
				// `-c"$C"` / `-n"$N"` is admitted only when the empty-value reading
				// cannot run a command. It then continues exactly as the non-empty
				// reading `-c2` does, so the child is still judged (#4460). The
				// scope keeps it out of commands that use #4465's options.
				flag, ok := ioniceDynamicValueFlag(words[0])
				if !ok || !scope.admitDynamicValue() || ioniceEmptyValueReadingLive(flag, words[1:]) {
					return nil, true
				}
				words = words[1:]
				continue
			}
			if !scope.admitExtension() {
				return nil, true
			}
			// A pinned token is self-contained, so its value is never judged as
			// a command head. The shadowed reading this file models forwards
			// whole argv words (`shift N; exec "$@"`); the token then execs as
			// `--classd=…`/`-c…`, never as env. Only a script that cuts the
			// value out of the word runs it, and such a script needs no argv at
			// all (`unset CODEX_HOME; exec codex`), so refusing the token would
			// close nothing (#4465 review, measured).
			words = words[1:]
			continue
		}
		if !scope.admitLiteralOption(option) {
			return nil, true
		}
		switch {
		case option == "--":
			return words[1:], false
		case utilLinuxTerminalOption(option, "tpPu"):
			// --help/--version exit before reaching a child on the real
			// binary, but the basename match cannot prove this IS that binary;
			// the words after the option still get inspected as a command.
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
		case ioniceProcessOnlyOption(option):
			// -p/-P/-u select existing-process modes that never exec a child
			// on real util-linux, so this external command cannot replace the
			// selected account environment inherited by one. The selector's
			// operand tail is still inspected rather than assumed inert:
			// isAccountCommandName matched the basename, which a PATH-shadowed
			// or repo-local `ionice` script satisfies while exec'ing the tail.
			// PID operands judge as an unrecognized literal command and stay
			// accepted; an env or shell tail is refused. Process-control policy
			// is outside this validator's environment-mutation contract.
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
		case option == "-t" || option == "--ignore":
			words = words[1:]
		case option == "-c" || option == "-n" || ioniceClassValueLongOption(option):
			if len(words) < 2 {
				return nil, true
			}
			// This value selects a scheduling class. It cannot move the child
			// boundary or touch the child's environment, so it only has to be
			// provably ONE argv word — it does not have to be literal. A
			// double-quoted scalar expansion always is, even expanding empty; an
			// unquoted one can word-split and shift the boundary, and "$@" can
			// produce several words, so both still fail closed.
			//
			// Measured on util-linux 2.39.3: an empty or unknown class exits with
			// "unknown scheduling class" before launching anything, and a valid one
			// goes on to --help or -p mode, so every runtime value of a single-word
			// operand leaves the following no-child modes reachable.
			if _, literal := literalShellWord(words[1]); !literal &&
				!isSimpleQuotedParameterWord(words[1]) {
				return nil, true
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			words = words[2:]
		case strings.HasPrefix(option, "-c") || strings.HasPrefix(option, "-n") ||
			ioniceClassValueLongOptionAttached(option):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return nil, true
		default:
			return words, false
		}
	}
	return nil, false
}

// literalPrefixBeforeSimpleQuotedParameter splits a word into a literal option
// prefix and a trailing simple quoted parameter expansion — `-c"$C"` gives
// "-c", true. The last part must be exactly one double-quoted scalar
// expansion; every earlier part must be literal.
func literalPrefixBeforeSimpleQuotedParameter(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) == 0 || !isSimpleQuotedParameterPart(word.Parts[len(word.Parts)-1]) {
		return "", false
	}
	var prefix strings.Builder
	for _, part := range word.Parts[:len(word.Parts)-1] {
		if !appendLiteralShellPart(&prefix, part) {
			return "", false
		}
	}
	return prefix.String(), true
}

func isSimpleQuotedParameterWord(word *syntax.Word) bool {
	prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(word)
	return dynamic && prefix == ""
}

func isSimpleQuotedParameterPart(part syntax.WordPart) bool {
	quoted, ok := part.(*syntax.DblQuoted)
	if !ok || quoted.Dollar || len(quoted.Parts) != 1 {
		return false
	}
	exp, ok := quoted.Parts[0].(*syntax.ParamExp)
	return ok && exp.Param != nil && shellParameterExpandsToOneWord(exp.Param.Value) &&
		exp.Flags == nil && exp.NestedParam == nil && exp.Index == nil &&
		len(exp.Modifiers) == 0 && exp.Slice == nil && exp.Repl == nil && exp.Exp == nil &&
		!exp.Excl && !exp.Length && !exp.Width && !exp.IsSet && exp.Names == 0
}

func shellParameterExpandsToOneWord(name string) bool {
	if validName(name) {
		return true
	}
	if name != "" && strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		return true
	}
	// Residual accepted set: scalar shell parameters whose double-quoted
	// expansion always occupies exactly one argv word, including when empty.
	// "$*" joins positional values into one word; "$@" is deliberately absent
	// because it expands to zero or many words. Structured/modifying parameter
	// expansions are rejected by the caller's remaining shape checks.
	return strings.Contains("!#$*-?", name) && len(name) == 1
}

// ioniceQuotedOptionBoundaryPinned reports whether an ionice option token whose
// value is a simple quoted expansion still occupies exactly one argv word for
// EVERY value that expansion can take, including the empty string.
//
// Two shapes pin it. A long option's literal '=' separates the value inside the
// same word, so `--class="$C"` is one word even when $C is empty. Literal value
// text after a short flag, as in `-c2"$X"`, proves the attached value is
// nonempty, so the flag cannot fall back to consuming the following word.
//
// A bare short flag with a wholly dynamic value, `-c"$C"`, is NOT pinned: when
// $C expands empty the word reduces to `-c`, and getopt then takes the
// FOLLOWING argv word as the class instead, which moves the child. Measured on
// util-linux 2.39.3 — with $C empty, `ionice -c"$C" /bin/echo X` reports
// `unknown scheduling class: '/bin/echo'` and execs nothing, while with $C=2
// the same command prints X. unwrapIonice admits that shape only through the
// #4460 proof in account_environment_ionice.go, which refuses it whenever the
// swallowed word could be a valid value.
func ioniceQuotedOptionBoundaryPinned(prefix string) bool {
	if strings.HasPrefix(prefix, "--") {
		name, _, attached := strings.Cut(prefix, "=")
		if !attached {
			return false
		}
		// util-linux resolves long-option prefixes, so an abbreviation of either
		// value-taking option counts. Both are value-taking, so an abbreviation
		// ambiguous between them still consumes exactly this one word.
		return strings.HasPrefix("--class", name) || strings.HasPrefix("--classdata", name)
	}
	if len(prefix) < 3 || prefix[0] != '-' {
		return false
	}
	return prefix[1] == 'c' || prefix[1] == 'n'
}

// ioniceClassValueLongOption reports whether option names one of ionice's two
// value-taking long options through a GNU long-option abbreviation. util-linux
// parses with getopt_long, which resolves any unambiguous prefix — so --classd,
// --classda and --classdat all spell --classdata, and the exact --class still
// wins over being a prefix of it. The only ambiguity a shorter spelling can
// hit is --class against --classdata, and BOTH take exactly one value word:
// like the selector ambiguity ioniceProcessOnlyOption documents, a spelling
// shared by same-arity candidates lands the child at the same word either way,
// so it is decidable without knowing which option was meant.
func ioniceClassValueLongOption(option string) bool {
	return len(option) > 2 &&
		(strings.HasPrefix("--class", option) || strings.HasPrefix("--classdata", option))
}

// ioniceClassValueLongOptionAttached is the `--opt=value` spelling of
// ioniceClassValueLongOption: the '=' pins the value inside this one argv word
// for every abbreviation getopt_long resolves.
func ioniceClassValueLongOptionAttached(option string) bool {
	name, _, attached := strings.Cut(option, "=")
	return attached && ioniceClassValueLongOption(name)
}

func ioniceProcessOnlyOption(option string) bool {
	// util-linux parses with getopt_long, so every nonempty prefix of a selector
	// names that selector, with or without an attached value. Ambiguity AMONG the
	// three selectors needs no resolution here: each of them switches ionice to
	// acting on already-running processes, so no resolution launches a child. A
	// prefix that is ambiguous with a non-selector, or unsupported by the
	// installed ionice, makes ionice exit before launching one — the same
	// no-child answer. This mirrors tasksetProcessOnlyOption, whose --pid prefix
	// handling landed for the same finding.
	//
	// Measured on util-linux 2.39.3: --pi, --pgi, --ui and --u all enter
	// process-only mode (as do their =value spellings), --p exits "ambiguous"
	// without a child, and --i resolves to --ignore and DOES exec its child, so
	// it must not match here.
	if name, _, _ := strings.Cut(option, "="); len(name) > 2 &&
		(strings.HasPrefix("--pid", name) ||
			strings.HasPrefix("--pgid", name) ||
			strings.HasPrefix("--uid", name)) {
		return true
	}
	if len(option) < 2 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for idx := 1; idx < len(option); idx++ {
		switch option[idx] {
		case 't':
			continue
		case 'p', 'P', 'u':
			return true
		case 'c', 'n':
			// These flags consume the remainder as their attached value.
			return false
		default:
			return false
		}
	}
	return false
}

// unwrapTaskset is unwrapIonice for `taskset`, with one extra step: taskset's
// first OPERAND is the affinity mask (or, after -c, the cpu list), and the
// command it runs begins only after it.
func unwrapTaskset(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	for len(words) > 0 {
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
					return nil, true
				}
				return words[1:], false
			}
			return nil, true
		}
		switch {
		case option == "--":
			return tasksetCommandAfterMask(words[1:], names, memo)
		case utilLinuxTerminalOption(option, "acp"):
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
		case tasksetProcessOnlyOption(option):
			// -p switches taskset from command execution to inspecting or
			// updating an existing PID, so no child environment exists to
			// mutate on the real binary. The operand tail is still inspected:
			// the basename match cannot distinguish taskset from a
			// PATH-shadowed script that execs whatever follows the selector.
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return nil, true
			}
			return words[1:], false
		case option == "-a" || option == "--all-tasks" ||
			option == "-c" || option == "--cpu-list":
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return nil, true
		default:
			return tasksetCommandAfterMask(words, names, memo)
		}
	}
	return nil, false
}

func utilLinuxTerminalOption(option, argumentFreeShortFlags string) bool {
	if len(option) > 2 && strings.HasPrefix(option, "--") {
		return strings.HasPrefix("--help", option) || strings.HasPrefix("--version", option)
	}
	if len(option) < 2 || option[0] != '-' || option[1] == '-' {
		return false
	}
	for _, flag := range option[1:] {
		if flag == 'h' || flag == 'V' {
			return true
		}
		// Only scan past argument-free flags. A value-taking flag owns the
		// rest of its argv word, so an h or V after it is operand text rather
		// than a terminal option.
		if !strings.ContainsRune(argumentFreeShortFlags, flag) {
			return false
		}
	}
	return false
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
	return words[1:], false
}

// unwrapXargs removes a GNU `xargs` prefix so the command it execs is what
// gets inspected. Unlike the other modeled wrappers, xargs splices content
// this walk cannot see into the child's argv: input items are appended after
// the initial arguments, and -I/-i/--replace substitutes every marker
// occurrence with an input line. An env invocation whose operand region can
// receive either is unprovable, because env re-parses the substituted word —
// an item spelling NAME=value becomes an assignment even when the marker sat
// in env's command slot.
func unwrapXargs(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	substituting := false
	markerKnown := true
	marker := "{}"
options:
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		switch {
		case option == "--":
			words = words[1:]
			break options
		case !strings.HasPrefix(option, "-") || option == "-":
			break options
		case strings.HasPrefix(option, "--"):
			name, value, attached := strings.Cut(option[2:], "=")
			switch name {
			case "help", "version":
				if shadowedOperandTailMutates(words[1:], names, memo) {
					return nil, true
				}
				return words[1:], false
			case "null", "interactive", "no-run-if-empty", "open-tty", "verbose", "exit", "show-limits":
				if attached {
					return nil, true
				}
				words = words[1:]
			case "eof", "max-lines":
				// Optional-argument long options take a value only via =.
				words = words[1:]
			case "replace":
				substituting = true
				if attached {
					marker = value
				}
				words = words[1:]
			case "arg-file", "delimiter", "max-args", "max-procs", "max-chars":
				if !attached {
					if len(words) < 2 {
						return nil, true
					}
					// The argument value itself is inert to this analysis on
					// the real binary, but a shadowed xargs may exec it.
					if wrapperOperandTailMutates(words[1:], names, memo) {
						return nil, true
					}
					words = words[1:]
				}
				words = words[1:]
			case "process-slot-var":
				var arg string
				var argLiteral bool
				if attached {
					arg, argLiteral = value, true
				} else {
					if len(words) < 2 {
						return nil, true
					}
					arg, argLiteral = literalShellWord(words[1])
					if wrapperOperandTailMutates(words[1:], names, memo) {
						return nil, true
					}
					words = words[1:]
				}
				if !argLiteral || accountEnvironmentOperandDenied(arg, names) {
					return nil, true
				}
				words = words[1:]
			default:
				return nil, true
			}
		default:
			flags := option[1:]
			for idx := 0; idx < len(flags); idx++ {
				switch flags[idx] {
				case '0', 'o', 'p', 'r', 't', 'x':
				case 'e', 'l':
					// -e/-l take an optional attached argument; whatever
					// remains in this word is the value.
					idx = len(flags)
				case 'i':
					substituting = true
					if idx+1 < len(flags) {
						marker = flags[idx+1:]
					}
					idx = len(flags)
				case 'a', 'd', 'E', 'I', 'L', 'n', 'P', 's':
					var arg string
					var argLiteral bool
					if idx+1 < len(flags) {
						arg, argLiteral = flags[idx+1:], true
					} else {
						if len(words) < 2 {
							return nil, true
						}
						arg, argLiteral = literalShellWord(words[1])
						if wrapperOperandTailMutates(words[1:], names, memo) {
							return nil, true
						}
						words = words[1:]
					}
					if flags[idx] == 'I' {
						substituting = true
						if argLiteral {
							marker = arg
						} else {
							markerKnown = false
						}
					}
					idx = len(flags)
				default:
					return nil, true
				}
			}
			words = words[1:]
		}
	}
	if len(words) == 0 {
		// With no command operand xargs runs its default echo on each input
		// item — nothing here to unwrap.
		return nil, false
	}
	if _, literal := literalShellWord(words[0]); !literal {
		return nil, true
	}
	for j := 0; j < len(words); j++ {
		if !isAccountCommandName(words[j], "env") {
			continue
		}
		invocation, err := envCallArgvParse(words[j+1:])
		if err != nil || invocation.ClearEnvironment {
			return nil, true
		}
		operandEnd := invocation.CommandIndex
		if operandEnd < 0 {
			operandEnd = len(words[j+1:])
		} else {
			// The command word counts as operand region: a substituted item
			// landing there is re-parsed by env, and an item spelling
			// NAME=value becomes an assignment rather than a program name.
			operandEnd++
		}
		if substituting {
			if !markerKnown {
				return nil, true
			}
			for k := 0; k < operandEnd; k++ {
				lit, ok := literalShellWord(words[j+1+k])
				if !ok {
					return nil, true
				}
				// Only the part before the first '=' is parsed as a name or
				// option; a marker in an assignment's value feeds data env
				// cannot reinterpret as a mutation.
				namePart, _, _ := strings.Cut(lit, "=")
				if strings.Contains(namePart, marker) {
					return nil, true
				}
			}
		} else if invocation.CommandIndex < 0 {
			// Without substitution, input items append after the initial
			// arguments — straight into env's operand region when env's own
			// argv names no command, so `xargs env` can run `env ITEM` with
			// ITEM spelling NAME=value.
			return nil, true
		}
	}
	return words, false
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

func letMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}, tainted map[string]struct{}) bool {
	for _, word := range words {
		expression, literal := literalShellWord(word)
		if !literal || accountSubscriptInArithmetic(expression, names) {
			return true
		}
		parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Arithmetic(strings.NewReader(expression))
		if err != nil {
			return true
		}
		// A command substitution (`$(...)` or backticks) inside a literal `let`
		// argument is unprovable: bash re-evaluates the substitution's stdout as
		// FRESH arithmetic before using it, so `arr[$(echo CODEX_HOME=1)]` runs
		// `echo CODEX_HOME=1`, splices `CODEX_HOME=1` back into the expression,
		// and evaluates it as an arithmetic assignment to a denied name. The
		// walk below judges the inner `echo` as inert data and never models that
		// re-evaluation; accountSubscriptInArithmetic only finds a literal
		// `name[`. Fail closed on any substitution the parser can see.
		if arithmeticExprHasCommandSubstitution(parsed) {
			return true
		}
		// A variable that was assigned from a command substitution earlier in the
		// same command is equally unprovable when referenced in arithmetic: bash
		// re-evaluates the variable's value as fresh arithmetic, so its prior
		// substitution output becomes a deferred arithmetic mutation.
		if arithmeticExprReferencesTaintedVar(parsed, tainted) {
			return true
		}
		mutates := false
		syntax.Walk(parsed, func(node syntax.Node) bool {
			if nodeMutatesAccountEnvironment(node, names, tainted) {
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

// arithmeticExprHasCommandSubstitution reports whether an arithmetic
// expression tree contains a command substitution (`$(...)` or backticks).
//
// bash re-evaluates the stdout of a command substitution as FRESH arithmetic
// before using it, including inside an array subscript that the parser reports
// as a plain read. The substitution can therefore print `NAME=value` and have
// bash execute it as an arithmetic assignment to a denied account-identity
// variable while the surrounding expression only appears to read it. The
// guard's own analysis judges the substitution's inner command (an `echo`) as
// inert data and never models the re-evaluation, and accountSubscriptInArithmetic
// only finds a literal `name[`, so neither can prove safety. Wherever a parsed
// arithmetic AST is treated as authoritative — `let`, `(( ))`, `$(( ))`, or a
// bash `let` clause — the presence of a command substitution makes the
// expression unprovable and the guard fails closed.
func arithmeticExprHasCommandSubstitution(expr syntax.ArithmExpr) bool {
	found := false
	syntax.Walk(expr, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// wordHasCommandSubstitution reports whether a shell word contains a command
// substitution (`$(...)` or backticks).
func wordHasCommandSubstitution(word syntax.Node) bool {
	found := false
	syntax.Walk(word, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// cmdSubstAssignedVars returns the set of variable names that are assigned
// (at the top-level, not inside a subshell) from command substitutions in the
// given file. These variables are "tainted": bash re-evaluates their value as
// fresh arithmetic when they appear inside an arithmetic context (`$(( ))`,
// `(( ))`, `let`, numeric `[[ ]]`), so a prior `x=$(printf CODEX_HOME=1)`
// followed by `: $((x))` carries the same bypass as an inline substitution.
//
// Taint propagates transitively through parameter-expansion copies: when `y=$x`
// and `x` is tainted, bash stores `x`'s contents in `y`, so `: $((y))` carries
// the same re-evaluation hazard. A single fixed-point pass over the same
// top-level Assign nodes handles chains of arbitrary length.
//
// Only top-level Assign nodes are collected; assignments inside a subshell or
// command substitution run in a child process and cannot affect the parent's
// environment, so they do not taint the outer scope.
func cmdSubstAssignedVars(file syntax.Node) map[string]struct{} {
	tainted := make(map[string]struct{})

	// First pass: collect direct command-substitution assignments.
	type assignRecord struct {
		name  string
		value *syntax.Word
	}
	var topLevelAssigns []assignRecord
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell, *syntax.CmdSubst:
			// Assignments inside a subshell or command substitution run in a
			// child process; they cannot affect the parent environment.
			return false
		case *syntax.Assign:
			if n.Name != nil && n.Value != nil {
				if wordHasCommandSubstitution(n.Value) {
					tainted[n.Name.Value] = struct{}{}
				} else {
					topLevelAssigns = append(topLevelAssigns, assignRecord{n.Name.Value, n.Value})
				}
			}
		}
		return true
	})

	// Second pass: propagate taint through parameter-expansion copies.
	// `y=$x` assigns x's value to y; if x is tainted, so is y. Repeat until
	// no new names are added (handles chains: x→y→z).
	for {
		added := false
		for _, rec := range topLevelAssigns {
			if _, already := tainted[rec.name]; already {
				continue
			}
			if wordReferencesTaintedVar(rec.value, tainted) {
				tainted[rec.name] = struct{}{}
				added = true
			}
		}
		if !added {
			break
		}
	}

	return tainted
}

// arithmeticExprReferencesTaintedVar reports whether an arithmetic expression
// tree contains a variable reference (bare word or `$name` expansion) of a
// tainted name. When a tainted variable appears inside arithmetic, bash
// re-evaluates that variable's value as fresh arithmetic — the same
// re-evaluation hazard as an inline command substitution — so the expression
// is unprovable.
//
// Array subscripts are NOT stripped: `arr[x]` evaluates `x` as arithmetic, so
// `x` must be checked against the tainted set in addition to `arr`. All
// identifiers inside `[…]` are extracted and tested. Additionally, `$name`
// expansions (ParamExp nodes) inside arithmetic are also checked, covering the
// `$(( $x ))` form.
func arithmeticExprReferencesTaintedVar(expr syntax.ArithmExpr, tainted map[string]struct{}) bool {
	if len(tainted) == 0 {
		return false
	}
	found := false
	syntax.Walk(expr, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.Word:
			name, literal := literalShellWord(n)
			if !literal {
				return true
			}
			// The word may be an array reference like `arr[x]`; bash evaluates
			// the subscript as arithmetic, so a tainted name inside `[…]` is
			// just as hazardous as the base name itself.
			if checkArithWordForTaint(name, tainted) {
				found = true
				return false
			}
		case *syntax.ParamExp:
			// `$name` inside arithmetic — bash expands the variable and then
			// re-evaluates its value as arithmetic, which is the same hazard
			// as a bare reference. Check the parameter name directly.
			if n.Param != nil {
				if _, taint := tainted[n.Param.Value]; taint {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// checkArithWordForTaint reports whether any shell identifier in the arithmetic
// word token (which may have the form `name`, `arr[idx]`, or `arr[a+b]`)
// appears in the tainted set.
//
// In arithmetic context, bash evaluates array subscripts as arithmetic too, so
// every identifier within `[…]` must be tested — not only the base array name.
func checkArithWordForTaint(word string, tainted map[string]struct{}) bool {
	// Collect every identifier in the word (base name and subscript names).
	// Identifiers are contiguous runs of [A-Za-z0-9_] not starting with a digit.
	start := -1
	checkIdent := func(s string) bool {
		if s == "" || (s[0] >= '0' && s[0] <= '9') {
			return false
		}
		_, taint := tainted[s]
		return taint
	}
	for i := 0; i <= len(word); i++ {
		if i < len(word) && isShellNameByte(word[i]) {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 && checkIdent(word[start:i]) {
				return true
			}
			start = -1
		}
	}
	return false
}

// wordReferencesTaintedVar reports whether a shell word (as used in a [[ ]]
// test operand) references a tainted variable name. bash re-evaluates the
// expanded value as arithmetic when the word appears in a numeric [[ ]]
// operand, so `[[ 0 -eq $x ]]` after `x=$(printf CODEX_HOME=1)` is the
// deferred form of the inline bypass.
//
// In bash arithmetic syntax, variable names can appear either as `$x`
// (ParamExp) or as the bare word `x` without `$`. A literal word that exactly
// matches a tainted variable name is therefore also a reference that must be
// refused.
func wordReferencesTaintedVar(word syntax.Node, tainted map[string]struct{}) bool {
	if len(tainted) == 0 {
		return false
	}
	found := false
	syntax.Walk(word, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.ParamExp:
			// `$x` form — explicit parameter expansion.
			if n.Param != nil {
				if _, taint := tainted[n.Param.Value]; taint {
					found = true
					return false
				}
			}
		case *syntax.Word:
			// Bare-word form — arithmetic syntax allows `x` (no `$`) as a
			// variable reference.  A literal word whose text matches a tainted
			// name is a reference to that variable.
			name, literal := literalShellWord(n)
			if literal {
				if checkArithWordForTaint(name, tainted) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
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
