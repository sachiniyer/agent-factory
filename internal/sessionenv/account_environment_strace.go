package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type straceOptionResult uint8

type straceOptionToken struct {
	literalPrefix               string
	quotedOperand               bool
	quotedOperandMayConsumeNext bool
}

const (
	straceOptionContinue straceOptionResult = iota
	straceOptionStops
	straceOptionUnsafe
	straceOptionDeferredUnsafe
	straceOptionAmbiguousValueBoundary
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
	return unwrapStraceState(words, false, names)
}

func unwrapStraceState(
	words []*syntax.Word,
	deferredUnsafe bool,
	names map[string]struct{},
) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		token, parsed := parseStraceOptionToken(words[0])
		if !parsed {
			return nil, true
		}
		option := token.literalPrefix
		if option == "--" {
			return words[1:], deferredUnsafe
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			return words, deferredUnsafe
		}

		var consumed int
		var result straceOptionResult
		if strings.HasPrefix(option, "--") {
			consumed, result = parseStraceLongOption(words, token, names)
		} else {
			consumed, result = parseStraceShortOptions(words, token, names)
		}
		switch result {
		case straceOptionContinue:
			words = words[consumed:]
		case straceOptionStops:
			// A terminal option exits during option parsing. Earlier semantic
			// hazards whose complete argv boundaries were known never take effect.
			return nil, false
		case straceOptionUnsafe:
			return nil, true
		case straceOptionDeferredUnsafe:
			deferredUnsafe = true
			words = words[consumed:]
		case straceOptionAmbiguousValueBoundary:
			return nil, straceAmbiguousValueBoundaryUnsafe(words, deferredUnsafe, names)
		}
	}
	return nil, deferredUnsafe
}

func parseStraceLongOption(
	words []*syntax.Word,
	token straceOptionToken,
	names map[string]struct{},
) (int, straceOptionResult) {
	value := token.literalPrefix
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
	if token.quotedOperand {
		if canonical == "--env" || canonical == "--output" {
			return 0, straceOptionUnsafe
		}
		return 1, straceOptionContinue
	}
	var operand string
	var consumed int
	var ok bool
	if canonical == "--env" || canonical == "--output" {
		operand, consumed, ok = straceOptionValue(words, attachedValue, attached)
	} else {
		operand, consumed, ok = straceOptionValueAllowQuotedScalar(words, attachedValue, attached)
	}
	if !ok {
		return 0, straceOptionUnsafe
	}
	switch canonical {
	case "--env":
		if straceEnvironmentMutationUnsafe(operand, names) {
			return consumed, straceOptionDeferredUnsafe
		}
	case "--output":
		if straceOutputTargetUnsafe(operand) {
			return consumed, straceOptionDeferredUnsafe
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

func parseStraceShortOptions(
	words []*syntax.Word,
	token straceOptionToken,
	names map[string]struct{},
) (int, straceOptionResult) {
	value := token.literalPrefix
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
			if token.quotedOperand {
				if flag == 'E' || flag == 'o' {
					return 0, straceOptionUnsafe
				}
				if token.quotedOperandMayConsumeNext {
					return 0, straceOptionAmbiguousValueBoundary
				}
				return 1, straceOptionContinue
			}
			var operand string
			var consumed int
			var ok bool
			if flag == 'E' || flag == 'o' {
				operand, consumed, ok = straceOptionValue(words, attachedValue, attached)
			} else {
				operand, consumed, ok = straceOptionValueAllowQuotedScalar(words, attachedValue, attached)
			}
			if !ok {
				return 0, straceOptionUnsafe
			}
			if flag == 'E' && straceEnvironmentMutationUnsafe(operand, names) {
				return consumed, straceOptionDeferredUnsafe
			}
			if flag == 'o' && straceOutputTargetUnsafe(operand) {
				return consumed, straceOptionDeferredUnsafe
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

func parseStraceOptionToken(word *syntax.Word) (straceOptionToken, bool) {
	if value, literal := literalShellWord(word); literal {
		return straceOptionToken{literalPrefix: value}, true
	}
	if word == nil || len(word.Parts) < 2 || !isSimpleQuotedParameterPart(word.Parts[len(word.Parts)-1]) {
		return straceOptionToken{}, false
	}
	var prefix strings.Builder
	for _, part := range word.Parts[:len(word.Parts)-1] {
		if !appendLiteralShellPart(&prefix, part) {
			return straceOptionToken{}, false
		}
	}
	consumes, mayConsumeNext := straceOptionConsumesQuotedSuffix(prefix.String())
	if !consumes {
		return straceOptionToken{}, false
	}
	return straceOptionToken{
		literalPrefix:               prefix.String(),
		quotedOperand:               true,
		quotedOperandMayConsumeNext: mayConsumeNext,
	}, true
}

func straceOptionConsumesQuotedSuffix(prefix string) (bool, bool) {
	if strings.HasPrefix(prefix, "--") {
		option, _, attached := strings.Cut(prefix, "=")
		canonical, result := classifyStraceLongOption(option)
		return attached && result == straceOptionContinue && canonical != "", false
	}
	if len(prefix) < 2 || prefix[0] != '-' {
		return false, false
	}
	for idx := 1; idx < len(prefix); idx++ {
		flag := prefix[idx]
		if flag == straceShortHelpOption || flag == straceShortVersionOption ||
			flag == straceShortOptionValueSeparator {
			return false, false
		}
		if strings.ContainsRune(straceShortOptionsWithSeparateValue, rune(flag)) {
			// A literal suffix after the option makes the attached value
			// provably nonempty. Without one, the quoted expansion may vanish;
			// getopt then consumes the following argv word as the value instead.
			return true, idx+1 == len(prefix)
		}
	}
	return false, false
}

func isSimpleQuotedParameterWord(word *syntax.Word) bool {
	if word == nil || len(word.Parts) != 1 {
		return false
	}
	return isSimpleQuotedParameterPart(word.Parts[0])
}

func isSimpleQuotedParameterPart(part syntax.WordPart) bool {
	quoted, ok := part.(*syntax.DblQuoted)
	if !ok || quoted.Dollar || len(quoted.Parts) != 1 {
		return false
	}
	exp, ok := quoted.Parts[0].(*syntax.ParamExp)
	return ok && exp.Param != nil && validName(exp.Param.Value) &&
		exp.Flags == nil && exp.NestedParam == nil && exp.Index == nil &&
		len(exp.Modifiers) == 0 && exp.Slice == nil && exp.Repl == nil && exp.Exp == nil &&
		!exp.Excl && !exp.Length && !exp.Width && !exp.IsSet && exp.Names == 0
}

func straceOptionValueAllowQuotedScalar(
	words []*syntax.Word,
	attachedValue string,
	attached bool,
) (string, int, bool) {
	operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
	if ok || attached || len(words) < 2 || !isSimpleQuotedParameterWord(words[1]) {
		return operand, consumed, ok
	}
	// A read-only scalar expansion inside double quotes is exactly one argv word,
	// even when empty, so it cannot move the child boundary.
	return "", 2, true
}

func straceAmbiguousValueBoundaryUnsafe(
	words []*syntax.Word,
	deferredUnsafe bool,
	names map[string]struct{},
) bool {
	// An attached value that is literal is self-contained. An attached value
	// produced solely by an expansion may be empty; a short option then consumes
	// the next argv word, so the token is not self-contained. Check both runtime
	// boundaries.
	if straceTailMutatesAccountEnvironment(words[1:], deferredUnsafe, names) {
		return true
	}
	if len(words) < 2 {
		// The empty branch leaves a required option value missing, so strace
		// exits before it can launch a child.
		return false
	}
	if _, literal := literalShellWord(words[1]); !literal && !isSimpleQuotedParameterWord(words[1]) {
		return true
	}
	return straceTailMutatesAccountEnvironment(words[2:], deferredUnsafe, names)
}

func straceTailMutatesAccountEnvironment(
	words []*syntax.Word,
	deferredUnsafe bool,
	names map[string]struct{},
) bool {
	child, unsafe := unwrapStraceState(words, deferredUnsafe, names)
	if unsafe {
		return true
	}
	return accountCommandWordsMutateEnvironment(child, names)
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
