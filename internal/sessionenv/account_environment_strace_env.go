package sessionenv

import (
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
		return 2, !ok || accountEnvironmentOperandDenied(value, names), false, true
	case strings.HasPrefix(literal, "-E"):
		return 1, accountEnvironmentOperandDenied(literal[2:], names), false, true
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

// dblQuotedExpandsToManyWords reports whether a DblQuoted part expands to more
// than one argv word. A double-quoted "$@" (or ${@}) expands to one word per
// positional parameter, and a double-quoted "${name[@]}" expands to one word
// per array element; both can word-split an option value into a further strace
// option. "$*" and "${name[*]}" join into one word and are excluded, as are
// length/width forms (${#@}) which collapse to a single word.
func dblQuotedExpandsToManyWords(quoted *syntax.DblQuoted) bool {
	for _, part := range quoted.Parts {
		exp, ok := part.(*syntax.ParamExp)
		if !ok || exp.Param == nil {
			continue
		}
		// "$@" / "${@}": the special @ parameter expands to one word per
		// positional parameter. ${#@} (length) collapses to one word.
		if exp.Param.Value == "@" && exp.Index == nil && !exp.Length && !exp.Width {
			return true
		}
		// "${name[@]}": an array indexed by @ expands to one word per
		// element. ${name[*]} joins into one word and is excluded.
		if w, ok := exp.Index.(*syntax.Word); ok && w.Lit() == "@" {
			return true
		}
	}
	return false
}
