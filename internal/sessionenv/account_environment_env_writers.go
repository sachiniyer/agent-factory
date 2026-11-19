package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unwrapSystemctlEnvironmentWriter judges systemctl by a lexical rule —
// deliberately NO option grammar: systemctl's option space is wide and
// every option-grammar spelling this arm tried was bypassed (operand
// before the writer, operand after it, the writer itself swallowed as an
// operand, a writer hidden after `--`, a dynamic subcommand). So: ANY
// non-literal word refuses (a dynamic word could BE the writer or hide
// one), ANY literal word naming a protected variable at an identifier
// boundary refuses, and the LAST `set-environment`/`import-environment`/
// `unset-environment` literal — the real subcommand, since earlier ones
// can only sit inside option operands — must be followed by a non-empty
// run of plain env names, else the write is bulk or unprovable and
// refuses. The name-list rule makes every swallowed-writer spelling
// land the same verdict: what survives the write's own tail is checked.
func unwrapSystemctlEnvironmentWriter(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	writer := -1
	for i, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			return nil, true
		}
		if unrecognizedWordProtectedKind(literal, names) != wordProtectedNone {
			return nil, true
		}
		if isSystemctlEnvironmentWriter(literal) {
			writer = i
		}
	}
	if writer >= 0 && !allLiteralEnvNames(words[writer+1:]) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, false
}

// unwrapDbusUpdateActivationEnvironment judges
// dbus-update-activation-environment by the same lexical rule: the
// command IS the writer, so every argv word must be a literal env name
// that isn't protected — `--all` bulk-imports the caller's ENTIRE
// environment into the shared activation block, a flag spelling can't
// be proven not to carry an operand, and an operand-less call has no
// name list to verify.
func unwrapDbusUpdateActivationEnvironment(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if !allLiteralEnvNames(words) {
		return nil, true
	}
	for _, word := range words {
		literal, _ := launcherLiteralWord(word)
		if unrecognizedWordProtectedKind(literal, names) != wordProtectedNone {
			return nil, true
		}
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, false
}

func isSystemctlEnvironmentWriter(literal string) bool {
	switch literal {
	case "set-environment", "import-environment", "unset-environment":
		return true
	}
	return false
}

// isLiteralEnvName reports whether a literal word is a plain environment
// NAME or NAME=value — the only operand shape an env-source write can
// carry. An option word, a non-identifier or a value-bearing name fails.
func isLiteralEnvName(literal string) bool {
	name, _, _ := strings.Cut(literal, "=")
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isEnvNameByte(name[i]) {
			return false
		}
	}
	return true
}

// allLiteralEnvNames reports whether words is a non-empty run of literal
// env names — a provable NAME list for an env-source write.
func allLiteralEnvNames(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok || !isLiteralEnvName(literal) {
			return false
		}
	}
	return true
}
