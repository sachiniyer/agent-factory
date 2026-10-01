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
			return "", false
		}
		resolved, _, known := resolveLongOption(name, straceLongOptions)
		if !known || resolved != "env" {
			return "", false
		}
		if !attached {
			return "", true
		}
		return val, true
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
		return "", false
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

// wordHasUnquotedExpansion reports whether word has a non-literal shell part
// that is subject to word splitting — a bare (unquoted) ParamExp, CmdSubst,
// ArithmExp, ProcSubst, &c. A non-literal expansion inside a DblQuoted or
// SglQuoted part stays one argv word (no splitting), so only the bare
// expansions let a value split into a further strace option.
func wordHasUnquotedExpansion(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		switch part.(type) {
		case *syntax.Lit, *syntax.DblQuoted, *syntax.SglQuoted:
			continue
		default:
			return true
		}
	}
	return false
}
