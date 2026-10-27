package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

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
	if answer, seen := memo.answers[words[0]]; seen {
		return answer
	}
	answer := wrapperOperandTailMutatesUncached(words, names, memo)
	memo.answers[words[0]] = answer
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
// past shadowedTailOperandLimit fails closed instead. That bound is a property
// of CHILDLESS tails: PIDs and permuted operands are meaningless past a handful
// of words, so length there is a reasonable fail-closed signal. A wrapper's
// returned child tail uses shadowedChildTailMutates instead, which drops it.
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
	run := memo.optionRun("nice")
	for len(words) > 0 {
		if cached, seen := run.visit(words, nil); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			return run.done(words[1:], false)
		case option == "-n" || option == "--adjustment":
			if len(words) < 2 {
				return run.done(nil, true)
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return run.done(nil, true)
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			words = words[2:]
		case strings.HasPrefix(option, "-n") || strings.HasPrefix(option, "--adjustment="):
			words = words[1:]
		case strings.HasPrefix(option, "-") && len(option) > 1:
			// Traditional nice accepts a bare numeric adjustment such as -10.
			if strings.Trim(option[1:], "0123456789") != "" {
				return run.done(nil, true)
			}
			words = words[1:]
		default:
			return run.done(words, false)
		}
	}
	return run.done(nil, false)
}

func unwrapTimeout(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("timeout")
	for len(words) > 0 {
		if cached, seen := run.visit(words, nil); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			words = words[1:]
			if len(words) < 2 {
				return run.done(nil, false)
			}
			if _, literal := literalShellWord(words[0]); !literal &&
				!isSimpleQuotedParameterWord(words[0]) {
				return run.done(nil, true)
			}
			if wrapperOperandTailMutates(words, names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case option == "-k" || option == "--kill-after" || option == "-s" || option == "--signal":
			if len(words) < 2 {
				return run.done(nil, true)
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return run.done(nil, true)
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			words = words[2:]
		case option == "--foreground" || option == "--preserve-status" || option == "-v" || option == "--verbose":
			words = words[1:]
		case strings.HasPrefix(option, "--kill-after=") || strings.HasPrefix(option, "--signal="):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return run.done(nil, true)
		default:
			if len(words) < 2 {
				return run.done(nil, false)
			}
			// The duration is an operand like taskset's mask: it stays a
			// candidate for the shadowed reading.
			if wrapperOperandTailMutates(words, names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		}
	}
	return run.done(nil, false)
}

func unwrapSetsid(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("setsid")
	for len(words) > 0 {
		if cached, seen := run.visit(words, nil); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			return run.done(nil, true)
		}
		switch option {
		case "--":
			return run.done(words[1:], false)
		case "-h", "--help", "-V", "--version":
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case "-c", "--ctty", "-f", "--fork", "-w", "--wait":
			words = words[1:]
		default:
			if strings.HasPrefix(option, "-") && len(option) > 1 {
				for _, flag := range option[1:] {
					if flag != 'c' && flag != 'f' && flag != 'w' {
						return run.done(nil, true)
					}
				}
				words = words[1:]
				continue
			}
			return run.done(words, false)
		}
	}
	return run.done(nil, false)
}

func unwrapStdbuf(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	run := memo.optionRun("stdbuf")
	for len(words) > 0 {
		if cached, seen := run.visit(words, nil); seen {
			return run.done(cached.words, cached.unsafe)
		}
		option, literal := literalShellWord(words[0])
		if !literal {
			return run.done(nil, true)
		}
		switch {
		case option == "--":
			return run.done(words[1:], false)
		case option == "--help" || option == "--version":
			if shadowedOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words[1:], false)
		case option == "-i" || option == "--input" ||
			option == "-o" || option == "--output" ||
			option == "-e" || option == "--error":
			if len(words) < 2 {
				return run.done(nil, true)
			}
			if _, literal := literalShellWord(words[1]); !literal {
				return run.done(nil, true)
			}
			if wrapperOperandTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			words = words[2:]
		case strings.HasPrefix(option, "--input=") ||
			strings.HasPrefix(option, "--output=") ||
			strings.HasPrefix(option, "--error="):
			words = words[1:]
		case len(option) > 2 && option[0] == '-' && strings.ContainsRune("ioe", rune(option[1])):
			words = words[1:]
		case strings.HasPrefix(option, "-"):
			return run.done(nil, true)
		default:
			return run.done(words, false)
		}
	}
	return run.done(nil, false)
}

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
			// `--` ends option parsing, so the real binary's child is exactly
			// words[1:]. The basename match cannot prove this IS real util-linux,
			// so a shadowed `./ionice` with `shift N; exec "$@"` can discard the
			// `--` and any prefix of the child and exec any literal suffix of it.
			// Every suffix is judged; because these words ARE the real child's
			// argv, the child-tail scan drops the childless PID bound — their
			// length is not a mutation (the selector and terminal branches keep
			// the capped shadowedOperandTailMutates for their childless tails).
			if shadowedChildTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
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
			// The child head word: option parsing has ended, so the real binary
			// runs words as COMMAND + args. The basename match cannot prove this
			// IS real util-linux, and a shadowed `./ionice` with
			// `shift N; exec "$@"` can discard any prefix of the child and exec
			// any literal suffix of it. The head itself is judged by the outer
			// unwrapAccountCommand loop that re-enters on this return; the tail
			// after the head is judged here, every suffix. Because this tail is
			// the real child's argv, it uses the child-tail scan that drops the
			// childless PID bound (a command may take any number of operands, so
			// length is not a mutation) — the selector and terminal branches keep
			// the capped shadowedOperandTailMutates for their childless tails.
			// This covers the non-terminal option branches (-t/--ignore, -c/-n/
			// --class/--classdata value, and the attached -c/-n forms) whose loops
			// land here once the real child is reached, while staying clear of
			// their future option words (#4460/#4532 dynamic `-c"$CLASS"`).
			if shadowedChildTailMutates(words[1:], names, memo) {
				return run.done(nil, true)
			}
			return run.done(words, false)
		}
	}
	return run.done(nil, false)
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
		// mvdan's arithmetic parser accepts a degenerate token such as `.` as a
		// nil AST with no error, and syntax.Walk panics on a nil node. A nil AST
		// carries nothing the walk can clear as inert, so treat it as unprovable
		// (fail closed) rather than walking it. Buried `let .` reaches here once
		// the child-tail suffix scan judges a `let` candidate (#4708).
		if parsed == nil {
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
