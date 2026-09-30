package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unwrapIonice removes an `ionice` prefix so the command it schedules is what
// gets inspected. util-linux ionice runs an arbitrary COMMAND after its
// options exactly as nice does, and this repository already models it as an
// executable wrapper (session/tmux/resume.go). Left unwrapped it read as an
// opaque leaf program whose arguments were inert, so
// `ionice -c 3 sh -c 'unset CODEX_HOME; codex'` reached the default-safe
// return and the nested shell removed the selected root before launch.
func unwrapIonice(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("ionice")
	var scope ioniceProofScope
	for len(words) > 0 {
		if cached, seen := run.visit(words, scope); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			// An option token carrying a quoted value is still ONE argv word when
			// a literal '=' or literal value text pins its boundary, so the value
			// need not be literal for the token to be understood. Besides pinned
			// tokens, only process selectors and the unpinned shape the #4460
			// proof covers (ioniceQuotedOptionBoundaryPinned) are accepted here.
			prefix, quoted := literalPrefixBeforeSimpleQuotedParameter(words[0])
			if !quoted {
				return run.done(nil, true)
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
					return run.done(nil, true)
				}
				return run.done(words[1:], false)
			}
			if !ioniceQuotedOptionBoundaryPinned(prefix) {
				// `-c"$C"` / `-n"$N"` is admitted only when the empty-value reading
				// cannot run a command. It then continues exactly as the non-empty
				// reading `-c2` does, so the child is still judged (#4460). The
				// scope keeps it out of commands that use #4465's options.
				flag, ok := ioniceDynamicValueFlag(words[0])
				if !ok || !scope.admitDynamicValue() || ioniceEmptyValueReadingLive(flag, words[1:]) {
					return run.done(nil, true)
				}
				words = words[1:]
				continue
			}
			if !scope.admitExtension() {
				return run.done(nil, true)
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
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			return run.done(words[1:], false)
		case utilLinuxTerminalOption(option, "tpPu"):
			// --help/--version exit before reaching a child on the real
			// binary, but the basename match cannot prove this IS that binary;
			// the words after the option still get inspected as a command.
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
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
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case option == "-t" || option == "--ignore":
			words = words[1:]
		case option == "-c" || option == "-n" || ioniceClassValueLongOption(option):
			if len(words) < 2 {
				return run.done(nil, true)
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
				return run.done(nil, true)
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			words = words[2:]
		case strings.HasPrefix(option, "-c") || strings.HasPrefix(option, "-n") ||
			ioniceClassValueLongOptionAttached(option):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return run.done(nil, true)
		default:
			return run.done(words, false)
		}
	}
	return run.done(nil, false)
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
