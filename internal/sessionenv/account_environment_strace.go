package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type straceOptionResult uint8

const (
	straceOptionContinue straceOptionResult = iota
	straceOptionStops
	straceOptionUnsafe
	// Help and version stop parsing separately because they never execute a
	// child. Every other short option that can consume the next argv word is in
	// this set; options absent from it are self-contained in their argv word.
	straceShortOptionsWithSeparateValue = "abeEIoOpPsSuUX"
	// A '=' ends a short-option cluster and begins an attached value. Nothing
	// after it can consume the following argv word.
	straceShortOptionValueSeparator = '='
	straceShortHelpOption           = 'h'
	straceShortVersionOption        = 'V'
)

// This is the complete long-option set that may take its value from the next
// argv word. The child boundary depends on these names (including GNU-style
// abbreviations), so an unresolved value fails closed. Every literal option
// that neither names nor abbreviates one of them is self-contained and advances
// one word: accepting an unfamiliar spelling can then only let strace accept it
// or reject it before launch; it cannot hide or replace the following child.
//
// Keep environment/output in this arity table too. Their attached values do not
// move the child boundary, but they have security semantics of their own and
// are checked after the shared value parser below.
var straceLongOptionsWithSeparateValue = map[string]struct{}{
	"--abbrev":                   {},
	"--argv0":                    {},
	"--attach":                   {},
	"--columns":                  {},
	"--color":                    {},
	"--const-print-style":        {},
	"--decode-pid":               {},
	"--decode-pids":              {},
	"--detach-on":                {},
	"--env":                      {},
	"--expr":                     {},
	"--fault":                    {},
	"--inject":                   {},
	"--interruptible":            {},
	"--kvm":                      {},
	"--output":                   {},
	"--raw":                      {},
	"--read":                     {},
	"--signal":                   {},
	"--signals":                  {},
	"--stack-trace-frame-limit":  {},
	"--status":                   {},
	"--string-limit":             {},
	"--summary-columns":          {},
	"--summary-sort-by":          {},
	"--summary-syscall-overhead": {},
	"--syscall-limit":            {},
	"--trace":                    {},
	"--trace-fd":                 {},
	"--trace-fds":                {},
	"--trace-path":               {},
	"--user":                     {},
	"--verbose":                  {},
	"--write":                    {},
}

// All entries above have required_argument and a nil flag in strace's
// getopt_long table. These cross-version spellings also share the same val, so
// glibc treats a common prefix as one match rather than as an ambiguity.
var straceLongEquivalentAliases = map[string]string{
	"--decode-pid": "--decode-pids",
	"--signal":     "--signals",
	"--trace-fd":   "--trace-fds",
}

// getopt_long gives an exact name precedence over abbreviations. These are the
// self-contained exact names that are also prefixes of a separate-value option;
// preserving that precedence keeps their following word visible as the child.
var straceLongSelfContainedPrefixCollisions = map[string]struct{}{
	"--stack-trace": {},
	"--summary":     {},
}

// unwrapStrace returns the command strace executes. Unlike an unclassified
// literal process, strace assigns executable meaning to one of its operands,
// so every word up to that operand must be understood before the child can be
// inspected. Unreduced words and separate option values fail closed; literal
// self-contained options cannot move the executable boundary and stay open to
// spellings added by other strace versions.
func unwrapStrace(words []*syntax.Word, names map[string]struct{}) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			return words[1:], false
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			return words, false
		}

		var consumed int
		var result straceOptionResult
		if strings.HasPrefix(option, "--") {
			consumed, result = parseStraceLongOption(words, names)
		} else {
			consumed, result = parseStraceShortOptions(words, names)
		}
		switch result {
		case straceOptionContinue:
			words = words[consumed:]
		case straceOptionStops:
			return nil, false
		case straceOptionUnsafe:
			return nil, true
		}
	}
	return nil, false
}

func parseStraceLongOption(words []*syntax.Word, names map[string]struct{}) (int, straceOptionResult) {
	value, _ := literalShellWord(words[0])
	option, attachedValue, attached := strings.Cut(value, "=")
	canonical, result := classifyStraceLongOption(option)
	switch result {
	case straceOptionStops:
		// These terminal options never launch the trailing command. An attached
		// value is either accepted by that strace version or rejected before
		// launch, so neither spelling needs a child-boundary decision.
		return 0, straceOptionStops
	case straceOptionUnsafe:
		return 0, straceOptionUnsafe
	}

	if canonical == "" {
		return 1, straceOptionContinue
	}
	operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
	if !ok {
		return 0, straceOptionUnsafe
	}
	switch canonical {
	case "--env":
		if straceEnvironmentMutationUnsafe(operand, names) {
			return 0, straceOptionUnsafe
		}
	case "--output":
		if straceOutputTargetUnsafe(operand) {
			return 0, straceOptionUnsafe
		}
	}
	return consumed, straceOptionContinue
}

func classifyStraceLongOption(option string) (string, straceOptionResult) {
	switch option {
	case "--help", "--version":
		return option, straceOptionStops
	}
	if _, takesSeparateValue := straceLongOptionsWithSeparateValue[option]; takesSeparateValue {
		return straceLongOptionFamily(option), straceOptionContinue
	}
	if _, exactSelfContained := straceLongSelfContainedPrefixCollisions[option]; exactSelfContained {
		return "", straceOptionContinue
	}

	// Prefix decision table:
	//   - no boundary-relevant family: self-contained, consume only this word;
	//   - one family (possibly several equivalent aliases): consume its value;
	//   - multiple inequivalent families: fail closed because arity/semantics
	//     cannot be selected safely.
	//
	// The open first result would accept a missing separate-value option, which
	// is why the installed-strace oracle test mechanically checks the table. The
	// closed last result can reject a prefix that is unique on a strace release
	// whose option set is smaller than this cross-version union; that is the
	// deliberate residual when the parser cannot prove which family applies.
	matchFamily := ""
	matchResult := straceOptionContinue
	conflict := false
	addMatch := func(family string, result straceOptionResult) {
		if matchFamily == "" {
			matchFamily = family
			matchResult = result
			return
		}
		if matchFamily != family || matchResult != result {
			conflict = true
		}
	}
	for candidate := range straceLongOptionsWithSeparateValue {
		if strings.HasPrefix(candidate, option) {
			addMatch(straceLongOptionFamily(candidate), straceOptionContinue)
		}
	}
	for _, candidate := range []string{"--help", "--version"} {
		if strings.HasPrefix(candidate, option) {
			addMatch(candidate, straceOptionStops)
		}
	}
	if conflict {
		return "", straceOptionUnsafe
	}
	if matchFamily == "" {
		return "", straceOptionContinue
	}
	return matchFamily, matchResult
}

func straceLongOptionFamily(option string) string {
	if family, alias := straceLongEquivalentAliases[option]; alias {
		return family
	}
	return option
}

func parseStraceShortOptions(words []*syntax.Word, names map[string]struct{}) (int, straceOptionResult) {
	value, _ := literalShellWord(words[0])
	for idx := 1; idx < len(value); idx++ {
		flag := value[idx]
		switch {
		case flag == straceShortOptionValueSeparator:
			return 1, straceOptionContinue
		case flag == straceShortHelpOption || flag == straceShortVersionOption:
			return 0, straceOptionStops
		case strings.ContainsRune(straceShortOptionsWithSeparateValue, rune(flag)):
			attached := idx+1 < len(value)
			attachedValue := ""
			if attached {
				attachedValue = value[idx+1:]
			}
			operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
			if !ok {
				return 0, straceOptionUnsafe
			}
			if flag == 'E' && straceEnvironmentMutationUnsafe(operand, names) {
				return 0, straceOptionUnsafe
			}
			if flag == 'o' && straceOutputTargetUnsafe(operand) {
				return 0, straceOptionUnsafe
			}
			return consumed, straceOptionContinue
		default:
			// Argument-free and unfamiliar flags are both self-contained. Keep
			// scanning because a later flag in the same cluster may take a value.
			continue
		}
	}
	return 1, straceOptionContinue
}

func straceOptionValue(words []*syntax.Word, attachedValue string, attached bool) (string, int, bool) {
	if attached {
		return attachedValue, 1, attachedValue != ""
	}
	if len(words) < 2 {
		return "", 0, false
	}
	value, literal := literalShellWord(words[1])
	return value, 2, literal
}

func straceOutputTargetUnsafe(operand string) bool {
	return strings.HasPrefix(operand, "|") || strings.HasPrefix(operand, "!")
}

func straceEnvironmentMutationUnsafe(operand string, names map[string]struct{}) bool {
	name, _, _ := strings.Cut(operand, "=")
	if !validName(name) {
		return true
	}
	return accountEnvironmentNameDenied(name, names)
}
