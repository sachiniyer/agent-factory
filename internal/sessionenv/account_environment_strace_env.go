package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// straceAttachedEnvOptionHides reports whether a non-literal tail word carries
// a strace -E/--env attached env option that sets or removes a protected
// variable. It is called only from wrapperTailWordHidesAccountAssignment's
// `!ok` branch (so the word is already known non-literal) and only while the
// scan is still inside strace's option region (the strace flag is set),
// mirroring the strace-only gate of the literal branch.
//
// literalShellWord discards the word's literal prefix on the first non-literal
// part, so an attached option whose value is a shell expansion never reaches
// the literal -E/--env handler. literalShellWordPrefix recovers that prefix,
// and straceAttachedEnvOperand recognizes every strace env-option spelling the
// repository's model accepts (account_environment_xargs_input.go and
// account_environment_getopt_tables.go): a short-option cluster whose first
// value-taking flag is E (e.g. -E, -fE) and the long --env option or its
// unambiguous abbreviation with an attached =value (e.g. --env=, --en=).
//
// Fail closed whenever the operand is not a settled, harmless variable:
//
//   - A fully non-provable operand (e.g. -E$V, --env=$V, -fE$V), or any operand
//     whose NAME — the text before the first '=', or the whole operand when no
//     '=' is present — is not fully literal, refuses: the expansion can name or
//     complete a protected variable (strace -ECODEX_$V=/x with V=HOME, or
//     -ECODEX_HOME$V with V unset removing the bare name), exactly as the
//     separate-word arm refuses a non-literal value word.
//   - A literal denied NAME refuses: the override occurs for any expansion of
//     the value, the empty expansion included, which af's resolvers read as the
//     ambient home.
//   - A literal non-denied NAME whose value holds an UNQUOTED expansion
//     refuses: unquoted expansion is subject to word splitting and can produce
//     a second -E option the scan never sees as a separate shell word
//     (strace -EFOO=$V with V="x -ECODEX_HOME=/y"). A QUOTED expansion stays one
//     option value and sets only the harmless NAME, so it stays allowed,
//     matching the attached literal -EFOO=1.
//
// This is strace-only: -E means extended-regexp to grep and others
// (account_environment_wrapper_test.go:250), and --env= belongs to strace
// only when its caller set the strace flag from the outermost wrapper.
func straceAttachedEnvOptionHides(word *syntax.Word, names map[string]struct{}) bool {
	operand, hasOperand := straceAttachedEnvOperand(word)
	if !hasOperand {
		return false
	}
	// The operand before any separator is non-literal (or entirely absent),
	// so its expansion can name or assign a protected variable. Fail closed
	// exactly as the separate-word arm does on a non-literal value word.
	if operand == "" {
		return true
	}
	// A '=' in the literal operand separates a fully literal NAME from the
	// value; the non-literal part sits in the value, after the separator.
	if strings.IndexByte(operand, '=') >= 0 {
		// An unquoted backslash escape stays in a syntax.Lit, so the raw
		// literal operand name is not shell-stable: strace -E\CODEX_HOME="$V"
		// literalizes the operand to \CODEX_HOME= (not denied) while the
		// shell removes the backslash and strace receives -ECODEX_HOME=<V>,
		// overriding the protected variable. Fail closed before trusting the
		// name; a backslash inside quotes is literal and excluded by
		// wordHasUnquotedBackslash.
		if wordHasUnquotedBackslash(word) {
			return true
		}
		if accountEnvironmentOperandDenied(operand, names) {
			return true
		}
		// Harmless literal name with a dynamic value: refuse unless the
		// value's expansion is quoted, so it cannot word-split into a
		// further -E option the scan does not see as a separate word.
		return wordHasUnquotedExpansion(word)
	}
	// No '=' in the literal operand: the NAME continues into the
	// non-literal tail, so it is not provable and can complete into any
	// denied name (or resolve to a bare denied removal) — fail closed.
	return true
}

// straceAttachedEnvOperand recognizes a strace -E/--env attached env option in
// word's literal prefix and returns the literal portion of its operand — the
// VAR[=VAL] text up to (not including) the first non-literal part. hasOperand
// is false when the prefix is not a strace env-option spelling. It reuses the
// same option tables straceInputRegion uses
// (account_environment_getopt_tables.go), so it recognizes short-option
// clusters whose first value-taking flag is E (e.g. -E, -fE) and the long
// --env option or its unambiguous abbreviation with an attached =value
// (e.g. --env=, --en=). A non-attached --env whose literal name resolves to
// env (e.g. --env$V, --en$V) is reported with an empty operand: the expansion
// can complete the option and the next word can name a protected variable, so
// the caller fails closed.
func straceAttachedEnvOperand(word *syntax.Word) (string, bool) {
	lit := literalShellWordPrefix(word)
	switch {
	case strings.HasPrefix(lit, "--"):
		name, val, attached := strings.Cut(lit[2:], "=")
		if name == "" {
			// An empty long option name (the literal prefix is "--")
			// followed by an expansion can complete into
			// --env=NAME[=value] (e.g. strace --"$V" codex with
			// V=env=CODEX_HOME=/other becomes the single argv word
			// --env=CODEX_HOME=/other), which the strace env-option
			// guard must catch. Fail closed with an empty operand.
			// This runs only on non-literal words, so a "--" prefix is
			// always "--" plus an expansion, never the literal "--"
			// terminator.
			return "", true
		}
		resolved, _, known := resolveLongOption(name, straceLongOptions)
		if !known || resolved != "env" {
			return "", false
		}
		if !attached {
			return "", true
		}
		return val, true
	// A literal prefix of just "-" is the traced-command boundary only when
	// the whole word is the bare "-"; this function is called solely on
	// non-literal words, so a "-" prefix is "-" followed by an expansion
	// (e.g. -"$V", -$V). The expansion can complete the option spelling into
	// -E/--env and then name or assign a protected variable
	// (strace -"$V" codex with V=ECODEX_HOME=/other becomes the single argv
	// word -ECODEX_HOME=/other), which is distinct from the handled -E$V
	// case: the expansion supplies the option letter, not only its operand.
	// Fail closed with an empty operand.
	case lit == "-":
		return "", true
	case strings.HasPrefix(lit, "-") && len(lit) > 1:
		flags := lit[1:]
		for i := 0; i < len(flags); i++ {
			if strings.IndexByte(straceShortValueFlags, flags[i]) < 0 {
				continue
			}
			if flags[i] != 'E' {
				return "", false
			}
			return flags[i+1:], true
		}
		// No value-taking flag appears in the literal cluster, so the
		// expansion that ends the literal prefix can complete the cluster
		// with a value-taking E (e.g. strace -f"$V" codex with
		// V=ECODEX_HOME=/other becomes the single argv word
		// -fECODEX_HOME=/other, which strace reads as -f -E
		// CODEX_HOME=/other). Fail closed with an empty operand; this
		// runs only on non-literal words, so such a cluster is always
		// followed by an expansion.
		return "", true
	default:
		return "", false
	}
}

// straceUnquotedOptionSplit reports whether word is a non-literal strace option
// word (a value-taking option whose value holds a shell expansion) whose value
// is an UNQUOTED expansion, so the expansion can word-split into a further
// strace option the scan never sees as a separate argv word — e.g.
// `strace -o$V` with V='out -ECODEX_HOME=/other' splits into `-oout` and
// `-ECODEX_HOME=/other`, the second an environment option the first-word scan
// misses. literalShellWordPrefix recovers the option marker; a bare "-" (the
// traced command) and a non-option word are excluded so this only fires for an
// option word. A QUOTED expansion stays one option value (no splitting) and
// returns false, so the quoted form keeps the tail judgment.
func straceUnquotedOptionSplit(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	prefix := literalShellWordPrefix(word)
	if !strings.HasPrefix(prefix, "-") || prefix == "-" {
		return false
	}
	return wordHasUnquotedExpansion(word)
}

// straceSeparateEnvOption reports whether literal is a strace -E/--env option
// that takes its var[=val] operand as the NEXT argv word — the separate-word
// form, as opposed to the attached -ENAME= or --env=NAME= form. It covers every
// spelling the repository's strace model recognizes: the exact -E/--env, a
// short-option cluster whose first value-taking flag is E at the cluster's end
// (-fE … = -f -E …), and the long --env abbreviation with no attached =value
// (--en …). The attached forms (-ENAME=, --env=NAME=) are recognized by the
// attached handler and return false here; a cluster whose E is followed by more
// flags has E's value attached in the cluster and is not a separate-word option.
func straceSeparateEnvOption(literal string) bool {
	switch {
	case strings.HasPrefix(literal, "--"):
		name, _, attached := strings.Cut(literal[2:], "=")
		if attached || name == "" {
			return false
		}
		resolved, requiresArg, known := resolveLongOption(name, straceLongOptions)
		return known && requiresArg && resolved == "env"
	case strings.HasPrefix(literal, "-") && len(literal) > 1:
		flags := literal[1:]
		for i := 0; i < len(flags); i++ {
			if strings.IndexByte(straceShortValueFlags, flags[i]) < 0 {
				continue
			}
			// The first value-taking flag consumes the rest of the
			// cluster as its value when it is not the last character
			// (attached, e.g. -fECODEX_HOME); only when it is E and sits
			// at the cluster's end is the value the next argv word.
			return flags[i] == 'E' && i+1 == len(flags)
		}
		return false
	default:
		return false
	}
}

// straceOptionAwaitsValue reports whether a literal strace option word takes
// its argument as the NEXT argv word (so that word is a value, not the traced
// command). It mirrors straceInputRegion's value-flag handling: a short-option
// cluster whose first value-taking flag is the cluster's last character
// (e.g. -o, -p, -e), and a long option that requires an argument and has no
// attached =value (e.g. --output, --signal). Attached forms (-ofoo,
// --output=f) and no-argument flags (-f) take no next word. It is used only to
// keep the strace option region from ending at a value word the traced command
// never reached.
func straceOptionAwaitsValue(literal string) bool {
	switch {
	case strings.HasPrefix(literal, "--"):
		name, _, attached := strings.Cut(literal[2:], "=")
		if attached || name == "" {
			return false
		}
		_, requiresArg, known := resolveLongOption(name, straceLongOptions)
		return known && requiresArg
	case strings.HasPrefix(literal, "-") && len(literal) > 1:
		flags := literal[1:]
		for i := 0; i < len(flags); i++ {
			if strings.IndexByte(straceShortValueFlags, flags[i]) < 0 {
				continue
			}
			return i+1 == len(flags)
		}
		return false
	default:
		return false
	}
}

// straceTracedWrapper reports whether word is a wrapper whose own options the
// tail scan analyzes when the wrapper is strace's traced command: strace itself
// (re-enters the option region), xargs (delegated to unwrapXargs), and
// systemd-run (delegated to the non-strace wrapper scan). Each is gated to
// strace's option region, so a "--" terminator that precedes one must not end
// the region — the next word is still the traced command, and ending the region
// there would skip the wrapper's env-mutating options.
func straceTracedWrapper(word *syntax.Word) bool {
	return isAccountCommandName(word, "strace") ||
		isAccountCommandName(word, "xargs") ||
		isAccountCommandName(word, "systemd-run")
}

// straceOptionWordHides judges a literal strace option word (still inside
// strace's option region) and reports how many words it consumes, whether it
// hides an account assignment, and the pendingValue the caller should carry
// for a value-taking option whose operand is the NEXT argv word. done is
// false only for the fall-through shape — a value-taking option whose next
// word is a literal or quoted value — so the caller can continue to the
// generic --opt=DENIED arm carrying pendingValue.
//
// It handles the three strace env-option spellings the wrapper tail must
// gate: the separate-word -E/--env form (and its cluster/abbreviation
// spellings) consuming the next word as var[=val], the attached -E NAME
// form, and a value-taking option whose separate operand is an UNQUOTED
// expansion that can word-split into a further strace option the scan never
// sees as a separate argv word (strace -o $V codex with
// V='trace -ECODEX_HOME=/other'). A QUOTED separate value stays one option
// value, so it is consumed as a pending value and keeps the tail judgment.
func straceOptionWordHides(literal string, words []*syntax.Word, names map[string]struct{}) (consumed int, hides, pendingValue, done bool) {
	switch {
	case straceSeparateEnvOption(literal):
		// strace's env option (-E/--env, a cluster ending in E such as
		// -fE, or the --en abbreviation) takes var[=val] as the NEXT argv
		// word and injects or REMOVES the variable in the traced child's
		// environment — the env arm's mutation in option spelling
		// (strace-only: -E is extended-regexp to grep). The value word is
		// consumed here, so it is not pending.
		if len(words) < 2 {
			return 1, true, false, true
		}
		value, ok := literalShellWord(words[1])
		// An unquoted backslash escape stays in a syntax.Lit, so the
		// raw literalShellWord value is not shell-stable: strace -E
		// \CODEX_HOME=/other codex literalizes the operand to
		// \CODEX_HOME=/other (not denied) while the shell removes the
		// backslash and strace receives -ECODEX_HOME=/other. Fail closed
		// when the operand word has such an escape before comparing the
		// name; a backslash inside quotes is literal and excluded.
		return 2, !ok || wordHasUnquotedBackslash(words[1]) ||
			accountEnvironmentOperandDenied(value, names), false, true
	case strings.HasPrefix(literal, "-E"):
		// An unquoted backslash escape stays in a syntax.Lit, so the raw
		// literal operand is not shell-stable: strace -E\CODEX_HOME=/other
		// literalizes the operand to \CODEX_HOME (not denied) while the
		// shell removes the backslash and strace receives
		// -ECODEX_HOME=/other. Fail closed when such an escape sits in the
		// NAME — the text before the first '=' — since only the NAME can
		// name a protected variable. A backslash in the VALUE (strace
		// -EFOO=\$V codex, which the shell passes as -EFOO=$V) names the
		// harmless FOO and must not block; a backslash inside quotes is
		// literal and excluded by wordHasUnquotedBackslashBeforeEq.
		return 1, wordHasUnquotedBackslashBeforeEq(words[0]) ||
			accountEnvironmentOperandDenied(literal[2:], names), false, true
	default:
		pendingValue = straceOptionAwaitsValue(literal)
		// A value-taking strace option whose operand is the NEXT argv word
		// consumes that word as its value. A non-literal value word with an
		// UNQUOTED expansion can word-split into a further strace option
		// the scan never sees as a separate argv word — e.g.
		// `strace -o $V codex` with V='trace -ECODEX_HOME=/other' is passed
		// as -o, trace, -ECODEX_HOME=/other, codex, and strace applies the
		// injected -E to the traced child. A QUOTED expansion stays one
		// option value, so it is consumed as a pending value below.
		if pendingValue && len(words) >= 2 && wordHasUnquotedExpansion(words[1]) {
			return 2, true, false, true
		}
		return 0, false, pendingValue, false
	}
}

// wordHasUnquotedExpansion reports whether word has a non-literal shell part
// that is subject to word splitting — a bare (unquoted) ParamExp, CmdSubst,
// ArithmExp, ProcSubst, &c. A non-literal expansion inside a DblQuoted or
// SglQuoted part stays one argv word (no splitting), so only the bare
// expansions let a value split into a further strace option. The exception is a
// double-quoted multi-value parameter expansion: "$@" and "${name[@]}" expand
// to one word per positional or array element even when quoted, so they can
// split an attached or pending option value into a further strace option the
// scan never sees as a separate argv word.
func wordHasUnquotedExpansion(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
			continue
		case *syntax.DblQuoted:
			if dblQuotedExpandsToManyWords(p) {
				return true
			}
			continue
		default:
			return true
		}
	}
	return false
}

// wordHasUnquotedBackslash reports whether word contains an unquoted backslash
// escape in a syntax.Lit part. literalShellWord keeps such a backslash in the
// Lit value (mvdan.cc/sh preserves the raw source text), but the shell removes a
// single backslash before the next character during expansion, so the
// literalShellWord value is not the argv value strace receives — e.g.
// `strace -E\CODEX_HOME=/other` literalizes the operand to
// `\CODEX_HOME=/other` (not denied) while strace receives
// `-ECODEX_HOME=/other` (denied). A backslash inside a DblQuoted or SglQuoted
// part is literal and stays, so only a top-level Lit is checked. The caller fails
// closed when this returns true, since the shell-stable operand name cannot be
// read from the literal and may name a protected variable.
func wordHasUnquotedBackslash(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		if lit, ok := part.(*syntax.Lit); ok && strings.ContainsRune(lit.Value, '\\') {
			return true
		}
	}
	return false
}

// wordHasUnquotedBackslashBeforeEq reports whether word contains an unquoted
// backslash escape (in a syntax.Lit part) that falls BEFORE the first '=' in
// the word's literal text. The shell removes an unquoted backslash, so a '\' in
// the NAME — the text before the first '=' — makes the literalShellWord NAME
// shell-unstable (strace -E\CODEX_HOME=/other literalizes the name to
// \CODEX_HOME while strace receives CODEX_HOME); a '\' AFTER the first '=' is
// in the VALUE and is shell-stable for a settled NAME (strace -EFOO=\$V codex
// passes -EFOO=$V, whose FOO name is harmless), so it must not block. The first
// '=' is tracked across parts, since the separator may sit in a quoted segment
// (strace -EFOO"="=\$V). A backslash inside an SglQuoted or DblQuoted part is
// literal and shell-stable, so only a top-level Lit is checked.
func wordHasUnquotedBackslashBeforeEq(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			for _, r := range p.Value {
				if r == '=' {
					return false
				}
				if r == '\\' {
					return true
				}
			}
		case *syntax.SglQuoted:
			if strings.ContainsRune(p.Value, '=') {
				return false
			}
		case *syntax.DblQuoted:
			if dblQuotedContainsRune(p, '=') {
				return false
			}
		}
	}
	return false
}

// dblQuotedContainsRune reports whether a DblQuoted part's literal text contains
// target. A DblQuoted that reaches this check carries only literal sub-parts
// (an expansion would have made the enclosing word non-literal), so its text
// is the concatenation of its syntax.Lit sub-parts.
func dblQuotedContainsRune(quoted *syntax.DblQuoted, target rune) bool {
	for _, part := range quoted.Parts {
		if lit, ok := part.(*syntax.Lit); ok && strings.ContainsRune(lit.Value, target) {
			return true
		}
	}
	return false
}

// dblQuotedExpandsToManyWords reports whether a DblQuoted part expands to more
// than one argv word. A double-quoted "$@" (or ${@}) expands to one word per
// positional parameter, and a double-quoted "${name[@]}" expands to one word
// per array element; both can word-split an option value into a further strace
// option. "$*" and "${name[*]}" join into one word and are excluded, as are
// length/width forms (${#@}) which collapse to a single word.
//
// A $@ or ${name[@]} nested inside a parameter expansion's alternative
// (${V:+$@}) or search/replace (${V/x/$@}) still splits under the outer double
// quote — the alternative's word is reached under the same quoting context — so
// the check recurses into those sub-words and any nested double quote.
func dblQuotedExpandsToManyWords(quoted *syntax.DblQuoted) bool {
	for _, part := range quoted.Parts {
		if wordPartExpandsToManyWords(part) {
			return true
		}
	}
	return false
}

// wordPartExpandsToManyWords reports whether a WordPart inside a double-quoted
// context can expand to more than one argv word. See dblQuotedExpandsToManyWords
// for the direct splitting forms and the recursion into nested alternatives.
func wordPartExpandsToManyWords(part syntax.WordPart) bool {
	switch p := part.(type) {
	case *syntax.ParamExp:
		return paramExpExpandsToManyWords(p)
	case *syntax.DblQuoted:
		return dblQuotedExpandsToManyWords(p)
	}
	return false
}

// paramExpExpandsToManyWords reports whether a ParamExp, reached under a
// double-quoted context, expands to more than one argv word. The direct forms
// are "$@"/"${@}" and "${name[@]}"; a nested $@/${name[@]} in the alternative
// (${V:+$@}) or replacement (${V/x/$@}) splits the same way, so recurse into
// those sub-words. Length/width forms (${#@}) collapse to one word and are
// excluded from the direct $@ check.
func paramExpExpandsToManyWords(exp *syntax.ParamExp) bool {
	if exp == nil || exp.Param == nil {
		return false
	}
	// "$@" / "${@}": one word per positional parameter. ${#@} (length) collapses.
	if exp.Param.Value == "@" && exp.Index == nil && !exp.Length && !exp.Width {
		return true
	}
	// "${name[@]}": one word per array element. ${name[*]} joins into one word.
	if w, ok := exp.Index.(*syntax.Word); ok && w.Lit() == "@" {
		return true
	}
	// A nested $@/${name[@]} in the alternative (${V:+$@}, ${V:-"$@"}) or
	// search/replace (${V/x/$@}) is still under the outer double quote.
	if exp.Exp != nil && wordExpandsToManyWords(exp.Exp.Word) {
		return true
	}
	if exp.Repl != nil && (wordExpandsToManyWords(exp.Repl.Orig) || wordExpandsToManyWords(exp.Repl.With)) {
		return true
	}
	return false
}

// wordExpandsToManyWords reports whether a Word reached under a double-quoted
// context carries a part that expands to more than one argv word. It recurses
// into a nested DblQuoted, whose contents stay quoted and still split on $@.
func wordExpandsToManyWords(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		if wordPartExpandsToManyWords(part) {
			return true
		}
	}
	return false
}

// unrecognizedWrapperHidesAccountAssignment and the wrapper tail scan live in this
// file alongside the strace -E/--env option helpers they drive: the unrecognized-wrapper
// tail is where strace's option region and value-taking options are modeled, so the
// helpers and the scan share one home.

// unrecognizedWrapperHidesAccountAssignment reports whether the literal tail
// words of an unrecognized argv-passthrough wrapper carry a NAME=... assignment
// whose NAME is one af removes for the selected account, specifically by
// nesting an env invocation inside the wrapper's argument list.
//
// unwrapAccountCommand peels a CLOSED list of wrappers (exec/command/builtin/
// nohup/nice/timeout/setsid/stdbuf/ionice/taskset/xargs); every other binary
// that runs a child command and passes argv through (strace/perf/valgrind/
// gdb --args/...) falls to the default arm and was returned opaque. Wrapping the
// modelled `env NAME=value <agent>` mutation — which the guard already refuses
// bare and under every modelled wrapper — in an unmodeled wrapper hid the inner
// assignment from the walk, so `strace env CODEX_HOME=/other codex` was accepted
// while `nohup env CODEX_HOME=/other codex` was refused.
//
// This lifts envCallMutatesAccountEnvironment's NAME= rule one level: scan
// the wrapper's literal argv tail for an `env` invocation and delegate to
// envCallMutatesAccountEnvironment when one is found. Commands that do not
// contain a nested env invocation are not refused even when an argument
// resembles a NAME=value token, so noun-uses such as `echo CODEX_HOME=/tmp`,
// `rg 'OPENAI_API_KEY='`, `man env`, `make env`, `git grep env`, `ls env/bin`,
// `pip show env`, or `strace -p 1234 env` (none of which carries a nested env
// invocation with a denied assignment) stay allowed.
//
// The same tail scan applies the modeled path's other two rules one level
// down: a shell word with argv is judged by shellCommandIsUnproven exactly as
// a bare `sh -c '...'` command is, and a literal option word whose VALUE is a
// denied name or assignment (xargs --process-slot-var=NAME, strace -E var=val
// and --env=var=val) is refused — a wrapper's own options can place the
// mutation without any env word.
//
// The scan is memoized per (position, strace): the verdict at a tail word
// depends only on the words from there on, so the tail of a wrapper nested in
// another wrapper's tail — `echo env env … env x` — is scanned once, not once
// per enclosing env word (#4966).
func unrecognizedWrapperHidesAccountAssignment(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	return wrapperTailHidesAccountAssignment(words[1:], isAccountCommandName(words[0], "strace"), names, memo)
}

// wrapperTailHidesAccountAssignment scans an unrecognized wrapper's tail from
// words[0]. Every position the scan steps on gets the scan's answer: the scan
// from there follows the same steps to the same end.
func wrapperTailHidesAccountAssignment(words []*syntax.Word, strace bool, names map[string]struct{}, memo operandTailMemo) bool {
	var chain []*syntax.Word
	var chainInOption []bool
	var chainPending []bool
	answer := false
	// strace parses options only up to its first non-option word (the traced
	// command) or "--"; a -E/--env-shaped word past that point is the
	// command's argument and cannot alter the traced environment. Track the
	// option region across the scan so the strace-specific arms — and the
	// generic --opt=DENIED arm, which is a wrapper-option check — apply only
	// to strace's options, not to the traced command's argv. (The env and
	// shell arms are not gated: a traced command that is itself env or a shell
	// is still judged by them.) pending marks the argv word a value-taking
	// strace option consumes as its value, so a value word (e.g. the file
	// after -o) is not mistaken for the traced command. Non-strace wrappers
	// never leave the region, preserving their existing scan.
	inOption := true
	pending := false
	for len(words) > 0 {
		if cached, seen := memo.wrapperTails[wrapperTailKey{word: words[0], strace: strace, inOption: inOption, pending: pending}]; seen {
			answer = cached
			break
		}
		chain = append(chain, words[0])
		chainInOption = append(chainInOption, inOption)
		chainPending = append(chainPending, pending)
		width, hides, boundary, pendingValue := wrapperTailWordHidesAccountAssignment(words, strace, inOption, pending, names, memo)
		if hides {
			answer = true
			break
		}
		switch {
		case pending:
			// This word is a value-taking strace option's value, not the
			// traced command; step past it and keep parsing strace options.
			pending = false
		case strace && boundary:
			inOption = false
		case strace && pendingValue:
			pending = true
		}
		words = words[width:]
	}
	for i, word := range chain {
		memo.wrapperTails[wrapperTailKey{word: word, strace: strace, inOption: chainInOption[i], pending: chainPending[i]}] = answer
	}
	return answer
}

// wrapperTailWordHidesAccountAssignment judges the tail word at words[0] and
// reports how many words it consumed. boundary reports that this word ends
// strace's option region (a literal non-option word — the traced command — or
// "--"); pendingValue reports that this word is a strace value-taking option
// whose value is the NEXT argv word (so that next word is a value, not the
// command). Both are meaningful only for a strace wrapper still inside its
// option region (strace && inOption); the caller uses them to stop applying
// the strace-specific and generic wrapper-option arms to the traced command's
// argv.
func wrapperTailWordHidesAccountAssignment(words []*syntax.Word, strace bool, inOption, pending bool, names map[string]struct{}, memo operandTailMemo) (int, bool, bool, bool) {
	word := words[0]
	if isAccountCommandName(word, "env") {
		// A nested env only mutates the child it execs; requireCommand
		// keeps an `env CODEX_HOME=/tmp` whose argv ends there — env's
		// print mode, which overrides nothing — allowed. env is judged by
		// this arm regardless of the strace option region (it is the traced
		// command under strace too), so it leaves the region as the caller
		// found it and any strace options env's argv re-enters are judged
		// the same way the pre-boundary scan judged them.
		return 1, envCallMutatesAccountEnvironment(words[1:], names, true, memo), false, false
	}
	literal, ok := literalShellWord(word)
	if !ok {
		// A pending word is the value a value-taking strace option (e.g. -o)
		// consumes as its next argv word, not a strace option itself: its
		// expansion is the option's value (a filename), so it cannot be a
		// -E/--env option and must not be judged by the attached-option or
		// split checks (strace -o "-ECODEX_HOME=$V" codex: the quoted word is
		// -o's output-file value, not a strace env option). Its unquoted form
		// is already caught when the option is recognized (strace -o $V), so a
		// pending value only reaches here as a quoted/literal value.
		if strace && inOption && !pending {
			if straceAttachedEnvOptionHides(word, names) {
				// The attached -E NAME[=value] and --env=NAME[=value] forms
				// whose value holds a shell expansion land here:
				// literalShellWord fails on the ParamExp/CmdSubst, discarding
				// the literal option marker and NAME that precede it. The
				// separate-word `-E <word>` and the attached literal
				// `-ENAME=` forms already fail closed; the non-literal
				// attached form must fail closed the same way. This is
				// strace-only: -E means extended-regexp to grep and others
				// (account_environment_wrapper_test.go:250), and --env=
				// belongs to strace only when its caller set the strace flag
				// from the outermost wrapper.
				return 1, true, false, false
			}
			if straceUnquotedOptionSplit(word) {
				// A non-literal strace option word (e.g. -o$V) whose value
				// is an UNQUOTED expansion can word-split into a further
				// strace option the scan never sees as a separate argv word;
				// fail closed (see straceUnquotedOptionSplit).
				return 1, true, false, false
			}
			if literalShellWordPrefix(word) == "" {
				// A fully non-literal word in strace's option region — no
				// literal prefix at all (e.g. strace "$V" codex or
				// strace $V codex) — can expand to any argv word, including a
				// -E/--env option that overrides the protected variable
				// (V=-ECODEX_HOME=/other). straceAttachedEnvOptionHides and
				// straceUnquotedOptionSplit both require a "-"-bearing
				// literal prefix, so such a word reached this default and was
				// accepted; fail closed the way the partial-prefix forms do.
				// A substituted xargs marker is excluded: it is a synthetic
				// stand-in for an input line the dedicated straceInputRegion
				// path already judges (a marker only in -E's value with no
				// traced command runs no child), so this arm must not
				// second-guess it.
				if !isXargsItemWord(word) {
					return 1, true, false, false
				}
			}
		}
		// An unprovable tail word can itself expand to `env` (or to a
		// multiword `env NAME=value` after word splitting); judge the
		// words after it as that invocation's argv. envScan admits a
		// substituted xargs marker after a literal env's command slot, but
		// this env is only a hypothesis, so a later marker is refused here as
		// a later "$x" is. A non-literal word is left in the option region:
		// its expansion can still be or complete a strace env option, so the
		// attached check stays armed for the words that follow it.
		return 1, memo.xargsItemFollows(words[1:]) ||
			envCallMutatesAccountEnvironment(words[1:], names, true, memo), false, false
	}
	if !strings.HasPrefix(literal, "-") || literal == "-" {
		// The traced command can itself be strace: `strace strace -E
		// CODEX_HOME=/other codex` runs an inner strace whose -E applies to
		// codex before the outer strace execs it, overriding the protected
		// variable while the scan treated the inner strace's argv as the
		// traced command's inert arguments. Re-enter the option region for
		// the inner strace instead of ending it, so the inner -E/--env is
		// judged by the same arms. A bare "-" is the traced-command
		// boundary (per straceInputRegion), not a re-entry, and a pending
		// value (strace -o strace ...) is the option's value, not a traced
		// strace.
		if strace && inOption && !pending && literal != "-" && isAccountCommandName(word, "strace") {
			return 1, false, false, false
		}
		// The traced command can itself be a modeled wrapper whose own
		// options mutate the child environment — xargs's
		// --process-slot-var=NAME sets NAME on every exec'd command, and
		// the generic --opt=DENIED arm below is gated to strace's option
		// region (a word past the boundary is the traced command's
		// argument, not a wrapper option). Without this delegation
		// strace xargs --process-slot-var=CODEX_HOME codex crossed the
		// boundary at xargs and the scan skipped the =DENIED check, so the
		// traced xargs set the protected variable unchecked. unwrapXargs
		// applies the same option analysis the modeled peel path does
		// (including its own --opt=DENIED refusal); a safe xargs whose
		// command is env is still caught by the env arm above, and a safe
		// xargs whose tail is a regular command stays allowed.
		if strace && inOption && !pending && literal != "-" && isAccountCommandName(word, "xargs") {
			_, unsafe := unwrapXargs(words[1:], names, memo)
			return 1, unsafe, strace && inOption, false
		}
		// The traced command can be an unmodeled wrapper that runs a child
		// and sets its environment through its OWN options — systemd-run's
		// --setenv=NAME[=VALUE] sets NAME on the transient service's child
		// (it documents `-E --setenv=NAME[=VALUE]` and runs the supplied
		// COMMAND). systemd-run is not a modeled peel wrapper, so unlike
		// xargs there is no unwrap helper; delegate its tail to the
		// non-strace wrapper scan, which applies the same generic
		// --opt=DENIED check a bare `systemd-run --setenv=CODEX_HOME=/other
		// codex` gets. A leaf command (echo, grep, …) is NOT delegated, so a
		// -E/--env-shaped argument of the traced command
		// (strace echo -ECODEX_HOME=$V codex) stays allowed — only a real
		// env-setting wrapper's options are inspected.
		if strace && inOption && !pending && literal != "-" && isAccountCommandName(word, "systemd-run") {
			return 1, wrapperTailHidesAccountAssignment(words[1:], false, names, memo), strace && inOption, false
		}
		// A shell in the wrapper's tail (`strace sh -c 'unset CODEX_HOME;
		// codex'`) gets the same verdict a bare shell command gets: the
		// modeled path refuses it under `nice sh -c ...` and only the
		// trusted account-shell form proves out. This non-option word is
		// the traced command under strace, ending the option region; a
		// bare "-" is one too (per straceInputRegion), so the second word
		// of `strace - -ECODEX_HOME=$V` is the traced program's argv, not a
		// strace env option.
		return 1, len(words) > 1 && knownShellName(filepath.Base(literal)) &&
			shellCommandIsUnproven(words), strace && inOption, false
	}
	// A literal option word. "--" ends strace's option region; any other
	// option keeps parsing options. boundary reports that boundary so the
	// caller stops applying the wrapper-option arms to the trailing argv.
	// "--" only ends strace's OWN option parsing: the NEXT word is still the
	// traced command, and when that command is itself a wrapper whose options
	// the scan analyzes (strace re-enters, xargs/systemd-run delegate), ending
	// the region at "--" would leave the wrapper's env-mutating options
	// uninspected (strace -- strace -E CODEX_HOME=/other codex,
	// strace -- xargs --process-slot-var=CODEX_HOME codex). Keep the region
	// open past "--" in that case so the wrapper's own arms fire on the next
	// word; a leaf command after "--" (strace -- echo -ECODEX_HOME=$V codex)
	// still ends the region and keeps its arguments allowed.
	boundary := strace && inOption && literal == "--" &&
		!(len(words) >= 2 && straceTracedWrapper(words[1]))
	pendingValue := false
	if strace && inOption {
		consumed, hides, pv, done := straceOptionWordHides(literal, words, names)
		if done {
			return consumed, hides, boundary, false
		}
		pendingValue = pv
	}
	// An unrecognized wrapper may expose options that mutate its child's
	// environment in option-value form — xargs's --process-slot-var=NAME
	// sets NAME on every exec'd command, and strace's --env=var=val is
	// analogous. A literal `--opt=DENIED` or `--opt=DENIED=value` is refused
	// when its value names a denied variable or carries a denied assignment.
	// This is a wrapper-option check: it runs for a non-strace wrapper's
	// options and for strace's options while the scan is still inside
	// strace's option region. Past strace's traced command a --opt-shaped
	// word is the command's argument, not a wrapper option, so a denied name
	// it carries stays allowed there.
	if !strace || inOption {
		if _, value, ok := strings.Cut(literal, "="); ok {
			return 1, accountEnvironmentOperandDenied(value, names), boundary, pendingValue
		}
	}
	return 1, false, boundary, pendingValue
}
