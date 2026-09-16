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

type straceOptionAction struct {
	consumed int
	result   straceOptionResult
}

type straceBoundaryEvaluation struct {
	memo map[straceBoundaryState]bool
}

type straceBoundaryState struct {
	first     *syntax.Word
	remaining int
	hazards   straceDeferredHazards
}

type straceDeferredHazards struct {
	environment bool
	output      bool
}

func (hazards straceDeferredHazards) affectChild() bool {
	return hazards.environment || hazards.output
}

const (
	straceOptionContinue straceOptionResult = iota
	straceOptionStops
	straceOptionUnsafe
	straceOptionDeferredEnvironmentUnsafe
	straceOptionDeferredOutputUnsafe
	// Help and version stop parsing separately because they never execute a
	// child. Entries in this set are stable arity proofs, not an exhaustive
	// parser: an absent short option produces both possible boundaries instead
	// of being guessed self-contained.
	straceShortOptionsWithSeparateValue = "abeEIoOpPsSuUX"
	// This proof keeps clustered -fp usable when p's quoted operand is the last
	// word. Omitting an argument-free flag merely preserves an extra boundary;
	// only a cross-version value-taking flag may not be added here.
	straceShortOptionsProvenSelfContained = "f"
	// A '=' ends a short-option cluster and begins an attached value. Nothing
	// after it can consume the following argv word.
	straceShortOptionValueSeparator = '='
	straceShortHelpOption           = 'h'
	straceShortVersionOption        = 'V'
)

// These are stable required-value arity proofs, not the accepted option model.
// A proof narrows a token to the boundary after its operand. Every bare option
// absent from this set keeps both the self-contained and separate-value
// boundaries alive, so omitting a new strace option can cause a conservative
// refusal but cannot hide its child. The installed-strace oracle verifies that
// these proofs agree with the host binary.
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

// All entries in straceLongOptionsWithSeparateValue have required_argument and
// a nil flag in strace's getopt_long table. These cross-version spellings also
// share the same val, so glibc treats a common prefix as one match rather than
// as an ambiguity.
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
// inspected. Unreduced words fail closed. A token with no stable arity proof is
// not represented as an "unknown self-contained option": the parser checks
// every child boundary it could have, while syntactically attached values keep
// their single fixed boundary without requiring an option catalogue.
func unwrapStrace(
	words []*syntax.Word,
	names map[string]struct{},
	evaluation *straceBoundaryEvaluation,
) ([]*syntax.Word, bool) {
	// The evaluation is supplied by the caller, not created here. A nested
	// strace reached through an ambiguous boundary would otherwise start an
	// empty memo, so both readings of every level would re-derive the same
	// suffixes and the cost would double per level.
	if evaluation == nil {
		evaluation = &straceBoundaryEvaluation{}
	}
	return unwrapStraceState(words, straceDeferredHazards{}, names, evaluation)
}

func unwrapStraceState(
	words []*syntax.Word,
	hazards straceDeferredHazards,
	names map[string]struct{},
	evaluation *straceBoundaryEvaluation,
) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		token, parsed := parseStraceOptionToken(words[0])
		if !parsed {
			return nil, true
		}
		option := token.literalPrefix
		if option == "--" {
			return words[1:], hazards.affectChild()
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			return words, hazards.affectChild()
		}

		var actions []straceOptionAction
		if strings.HasPrefix(option, "--") {
			actions = parseStraceLongOption(words, token, names)
		} else {
			actions = parseStraceShortOptions(words, token, names)
		}
		if len(actions) != 1 {
			return nil, straceOptionActionsUnsafe(words, actions, hazards, names, evaluation)
		}
		action := actions[0]
		if action.consumed > len(words) {
			// A required operand is absent, so option parsing exits before any
			// child or executable output target can be launched.
			return nil, false
		}
		switch action.result {
		case straceOptionContinue:
			words = words[action.consumed:]
		case straceOptionStops:
			// A terminal option exits during option parsing. Earlier semantic
			// hazards whose complete argv boundaries were known never take effect.
			return nil, false
		case straceOptionUnsafe:
			return nil, true
		case straceOptionDeferredEnvironmentUnsafe:
			hazards.environment = true
			words = words[action.consumed:]
		case straceOptionDeferredOutputUnsafe:
			hazards.output = true
			words = words[action.consumed:]
		}
	}
	// Environment options apply only to an executed tracee. With no child there
	// is nothing to modify, while an executable output target still launches for
	// attach mode and therefore remains unsafe.
	return nil, hazards.output
}

func parseStraceLongOption(
	words []*syntax.Word,
	token straceOptionToken,
	names map[string]struct{},
) []straceOptionAction {
	value := token.literalPrefix
	option, attachedValue, attached := strings.Cut(value, "=")
	canonical, result, exact := classifyStraceLongOption(option)
	switch result {
	case straceOptionStops:
		// These terminal options never launch the trailing command. An attached
		// value is either accepted by that strace version or rejected before
		// launch, so neither spelling needs a child-boundary decision.
		actions := []straceOptionAction{{result: straceOptionStops}}
		if !exact {
			actions = append(actions, straceOptionAction{consumed: 1, result: straceOptionContinue})
		}
		return actions
	case straceOptionUnsafe:
		return []straceOptionAction{{result: straceOptionUnsafe}}
	}

	if canonical == "" {
		if attached {
			// An option this model cannot name may still carry an environment
			// mutation in its attached value — the --opt=DENIED shape #4261
			// refuses generically on unmodeled wrappers (xargs's
			// --process-slot-var=NAME). Nothing here proves the value is not a
			// denied name or assignment, so it fails closed. Options the table
			// above DOES name keep their modelled semantics instead: their values
			// are syscall sets, paths and limits, and --env/--output are judged by
			// parseStraceSemanticOptionValue, so a denied-looking --trace= value
			// is not an environment mutation and is not refused for resembling one.
			if accountEnvironmentOperandDenied(attachedValue, names) {
				return []straceOptionAction{{result: straceOptionUnsafe}}
			}
			return []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
		}
		if exact {
			return []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
		}
		return stracePossibleValueBoundaries()
	}
	if canonical == "--env" || canonical == "--output" {
		// May return BOTH boundaries when an attached expansion can vanish.
		return parseStraceSemanticOptionValue(words, token, attachedValue, attached, canonical, names)
	}
	if token.quotedOperand {
		return []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
	}
	_, consumed, ok := straceOptionValueAllowQuotedScalar(words, attachedValue, attached)
	if !ok {
		return []straceOptionAction{{result: straceOptionUnsafe}}
	}
	return []straceOptionAction{{consumed: consumed, result: straceOptionContinue}}
}

func classifyStraceLongOption(option string) (string, straceOptionResult, bool) {
	switch option {
	case "--help", "--version":
		return option, straceOptionStops, true
	}
	if _, takesSeparateValue := straceLongOptionsWithSeparateValue[option]; takesSeparateValue {
		return straceLongOptionFamily(option), straceOptionContinue, true
	}
	if _, exactSelfContained := straceLongSelfContainedPrefixCollisions[option]; exactSelfContained {
		return "", straceOptionContinue, true
	}

	// Prefix decision table:
	//   - no boundary-relevant family: self-contained, consume only this word;
	//   - one family (possibly several equivalent aliases): return its canonical
	//     name so the caller applies its stable or versioned arity;
	//   - multiple inequivalent families: fail closed because arity/semantics
	//     cannot be selected safely.
	//
	// The open first result would accept a missing separate-value option, which
	// is why the installed-strace oracle test mechanically checks the table. The
	// closed last result can reject a prefix that is unique on a strace release
	// whose option set is smaller than this cross-version union; that is the
	// deliberate residual when the parser cannot prove which family applies.
	// Several inequivalent families can share a prefix. They are still resolvable
	// when every candidate puts the child at the same word: all members of
	// straceLongOptionsWithSeparateValue consume exactly one separate operand, so
	// an abbreviation matching only those has a provable boundary even though the
	// option itself is not proven. --env/--output are excluded because their
	// operand, not their arity, carries the security meaning, and a prefix that
	// could reach one of them cannot be given those semantics on a guess.
	// Differing RESULTS (a separate-value option against terminal --help) keep
	// failing closed, as does a prefix strace itself would call ambiguous when one
	// of the candidates is security-sensitive.
	matchFamily := ""
	matchResult := straceOptionContinue
	families := map[string]struct{}{}
	conflict := false
	semantic := false
	addMatch := func(family string, result straceOptionResult) {
		if _, seen := families[family]; !seen {
			families[family] = struct{}{}
		}
		if family == "--env" || family == "--output" {
			semantic = true
		}
		if matchFamily == "" {
			matchFamily = family
			matchResult = result
			return
		}
		if matchResult != result {
			conflict = true
		}
		if family < matchFamily {
			// Smallest name wins so the canonical result never depends on map
			// iteration order.
			matchFamily = family
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
	if conflict || (semantic && len(families) > 1) {
		return "", straceOptionUnsafe, false
	}
	if matchFamily == "" {
		return "", straceOptionContinue, false
	}
	return matchFamily, matchResult, false
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
) []straceOptionAction {
	return parseStraceShortOptionAt(words, token, names, 1)
}

func parseStraceShortOptionAt(
	words []*syntax.Word,
	token straceOptionToken,
	names map[string]struct{},
	idx int,
) []straceOptionAction {
	// Scanned iteratively rather than one frame per option byte. A cluster is
	// attacker-sized: an account-scoped request body may carry ~16MiB
	// (daemon.maxHTTPBodyBytes), and a per-byte recursion overflows the goroutine
	// stack at roughly 7M bytes. That is a FATAL runtime error, not a panic, so
	// no recover() in the request path can contain it — the whole daemon dies
	// with every live session, instead of the tab request being refused.
	//
	// Only the unmodeled arm below accumulates anything, and what it accumulates
	// collapses: it appends {1,continue} for a byte that is not the cluster's
	// last, {2,continue} for one that is, and appendUniqueStraceAction dedupes.
	// However long the cluster, at most those two actions survive, so the scan
	// carries two booleans instead of a stack.
	value := token.literalPrefix
	unmodeledBeforeLast := false
	unmodeledAtLast := false
	var actions []straceOptionAction
scan:
	for {
		if idx >= len(value) {
			actions = []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
			break
		}
		flag := value[idx]
		switch {
		case flag == straceShortOptionValueSeparator:
			// Every flag that reaches the separator is argument-free or unmodeled —
			// a value-taking flag consumes the rest of the word itself and never
			// reaches here — so nothing proves the attached value is not the
			// environment mutation #4261 refuses generically on unmodeled wrappers.
			if accountEnvironmentOperandDenied(value[idx+1:], names) {
				actions = []straceOptionAction{{result: straceOptionUnsafe}}
				break scan
			}
			actions = []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
			break scan
		case flag == straceShortHelpOption || flag == straceShortVersionOption:
			actions = []straceOptionAction{{result: straceOptionStops}}
			break scan
		case strings.ContainsRune(straceShortOptionsProvenSelfContained, rune(flag)):
			idx++
			continue
		case strings.ContainsRune(straceShortOptionsWithSeparateValue, rune(flag)):
			attached := idx+1 < len(value)
			attachedValue := ""
			if attached {
				attachedValue = value[idx+1:]
			}
			if flag == 'E' {
				actions = parseStraceSemanticOptionValue(
					words, token, attachedValue, attached, "--env", names,
				)
				break scan
			}
			if flag == 'o' {
				actions = parseStraceSemanticOptionValue(
					words, token, attachedValue, attached, "--output", names,
				)
				break scan
			}
			if token.quotedOperand {
				if token.quotedOperandMayConsumeNext {
					actions = stracePossibleValueBoundaries()
					break scan
				}
				actions = []straceOptionAction{{consumed: 1, result: straceOptionContinue}}
				break scan
			}
			_, consumed, ok := straceOptionValueAllowQuotedScalar(words, attachedValue, attached)
			if !ok {
				actions = []straceOptionAction{{result: straceOptionUnsafe}}
				break scan
			}
			actions = []straceOptionAction{{consumed: consumed, result: straceOptionContinue}}
			break scan
		default:
			// An unmodeled short flag is never assigned a guessed arity. One branch
			// treats it as argument-free and keeps parsing the cluster; the other
			// treats the rest of this word, or the following word when it is last,
			// as its operand. Stable arity proofs above only remove impossible
			// branches; omissions therefore fail closed rather than hiding a child.
			if idx+1 == len(value) {
				unmodeledAtLast = true
			} else {
				unmodeledBeforeLast = true
			}
			idx++
			continue
		}
	}
	// The recursion these two replace unwound deepest-first, so the last byte's
	// action is appended before the earlier bytes' shared one.
	if unmodeledAtLast {
		actions = appendUniqueStraceAction(actions, straceOptionAction{
			consumed: 2,
			result:   straceOptionContinue,
		})
	}
	if unmodeledBeforeLast {
		actions = appendUniqueStraceAction(actions, straceOptionAction{
			consumed: 1,
			result:   straceOptionContinue,
		})
	}
	return actions
}

func stracePossibleValueBoundaries() []straceOptionAction {
	return []straceOptionAction{
		{consumed: 1, result: straceOptionContinue},
		{consumed: 2, result: straceOptionContinue},
	}
}

func appendUniqueStraceAction(
	actions []straceOptionAction,
	action straceOptionAction,
) []straceOptionAction {
	for _, existing := range actions {
		if existing == action {
			return actions
		}
	}
	return append(actions, action)
}

func parseStraceOptionToken(word *syntax.Word) (straceOptionToken, bool) {
	if value, literal := literalShellWord(word); literal {
		return straceOptionToken{literalPrefix: value}, true
	}
	prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(word)
	if !dynamic {
		return straceOptionToken{}, false
	}
	consumes, mayConsumeNext := straceOptionConsumesQuotedSuffix(prefix)
	if !consumes {
		return straceOptionToken{}, false
	}
	return straceOptionToken{
		literalPrefix:               prefix,
		quotedOperand:               true,
		quotedOperandMayConsumeNext: mayConsumeNext,
	}, true
}

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

func straceOptionConsumesQuotedSuffix(prefix string) (bool, bool) {
	if strings.HasPrefix(prefix, "--") {
		_, _, attached := strings.Cut(prefix, "=")
		// Residual accepted set: any long option with a literal '=' followed by
		// one simple quoted scalar. The '=' fixes the next-word boundary even
		// when the expansion is empty. Long words without that literal boundary,
		// and dynamic option names, remain unparsed and fail closed.
		return attached, false
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

// straceBareOptionNextWordResult judges the operand a BARE semantic option takes
// from the following argv word — the reading that applies when an attached
// quoted expansion turns out to be empty. It mirrors the literal and
// simple-quoted handling the non-vanishing path uses, and fails closed on a word
// whose shape proves neither.
func straceBareOptionNextWordResult(
	word *syntax.Word,
	semantic string,
	names map[string]struct{},
) straceOptionResult {
	if operand, literal := literalShellWord(word); literal {
		return straceLiteralSemanticResult(semantic, operand, names)
	}
	if prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(word); dynamic {
		return straceDynamicSemanticResult(semantic, prefix, names)
	}
	return straceOptionUnsafe
}

func parseStraceSemanticOptionValue(
	words []*syntax.Word,
	token straceOptionToken,
	attachedValue string,
	attached bool,
	semantic string,
	names map[string]struct{},
) []straceOptionAction {
	if token.quotedOperand {
		result := straceDynamicSemanticResult(semantic, attachedValue, names)
		if token.quotedOperandMayConsumeNext {
			// The attached expansion may vanish, leaving a BARE option that
			// consumes the FOLLOWING word as its value instead. Neither boundary
			// is disprovable, so keep both — exactly as an unmodeled option does —
			// rather than refusing the token outright.
			//
			// Each boundary is judged by ITS OWN operand. On the nonempty reading
			// that is the attached expansion; on the empty reading the expansion is
			// gone and the operand is words[1], which is frequently provable when
			// the expansion was not. `strace -o"$OUT" --version` turns on exactly
			// that: the empty reading makes --version an ordinary output filename,
			// not an executable `|`/`!` target, so that boundary carries no hazard
			// at all. Giving both boundaries the attached value's verdict would
			// refuse it for a hazard the second reading cannot have.
			actions := []straceOptionAction{{consumed: 1, result: result}}
			if len(words) > 1 {
				actions = append(actions, straceOptionAction{
					consumed: 2,
					result:   straceBareOptionNextWordResult(words[1], semantic, names),
				})
			}
			return actions
		}
		return []straceOptionAction{{consumed: 1, result: result}}
	}
	operand, consumed, literal := straceOptionValue(words, attachedValue, attached)
	if literal {
		return []straceOptionAction{{
			consumed: consumed,
			result:   straceLiteralSemanticResult(semantic, operand, names),
		}}
	}
	if len(words) < 2 {
		return []straceOptionAction{{consumed: 2, result: straceOptionContinue}}
	}
	if attached {
		return []straceOptionAction{{result: straceOptionUnsafe}}
	}
	prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(words[1])
	if !dynamic {
		return []straceOptionAction{{result: straceOptionUnsafe}}
	}
	return []straceOptionAction{{
		consumed: 2,
		result:   straceDynamicSemanticResult(semantic, prefix, names),
	}}
}

func straceLiteralSemanticResult(
	semantic string,
	operand string,
	names map[string]struct{},
) straceOptionResult {
	switch semantic {
	case "--env":
		if straceEnvironmentMutationUnsafe(operand, names) {
			return straceOptionDeferredEnvironmentUnsafe
		}
	case "--output":
		if straceOutputTargetUnsafe(operand) {
			return straceOptionDeferredOutputUnsafe
		}
	}
	return straceOptionContinue
}

func straceDynamicSemanticResult(
	semantic string,
	literalPrefix string,
	names map[string]struct{},
) straceOptionResult {
	// Accepted dynamic semantic values are deliberately bounded: environment
	// values need a complete literal NAME= prefix, and output targets need a
	// literal first byte. Anything else is deferred rather than refused here:
	// an unprovable value is a hazard only when a child (or, for an output
	// target, an attach) actually launches, so a later terminal option or a
	// childless attach must still clear it, exactly like the literal hazard
	// forms. A surviving deferred hazard still fails closed on any executed
	// child, keeping forms such as --env="$SPEC" and -o"$PATH" refused.
	switch semantic {
	case "--env":
		name, _, fixedName := strings.Cut(literalPrefix, "=")
		if !fixedName || !validName(name) {
			return straceOptionDeferredEnvironmentUnsafe
		}
		if accountEnvironmentNameDenied(name, names) {
			return straceOptionDeferredEnvironmentUnsafe
		}
	case "--output":
		if literalPrefix == "" {
			return straceOptionDeferredOutputUnsafe
		}
		if straceOutputTargetUnsafe(literalPrefix) {
			return straceOptionDeferredOutputUnsafe
		}
	}
	return straceOptionContinue
}

func straceOptionValueAllowQuotedScalar(
	words []*syntax.Word,
	attachedValue string,
	attached bool,
) (string, int, bool) {
	operand, consumed, ok := straceOptionValue(words, attachedValue, attached)
	if !attached && len(words) < 2 {
		// Preserve the missing-operand boundary. The action evaluator recognizes
		// that it lies past argv and therefore launches no child.
		return "", 2, true
	}
	if ok || attached || len(words) < 2 || !isSimpleQuotedParameterWord(words[1]) {
		return operand, consumed, ok
	}
	// A read-only scalar expansion inside double quotes is exactly one argv word,
	// even when empty, so it cannot move the child boundary.
	return "", 2, true
}

func straceOptionActionsUnsafe(
	words []*syntax.Word,
	actions []straceOptionAction,
	hazards straceDeferredHazards,
	names map[string]struct{},
	evaluation *straceBoundaryEvaluation,
) bool {
	for _, action := range actions {
		if action.consumed > len(words) {
			continue
		}
		branchHazards := hazards
		switch action.result {
		case straceOptionStops:
			continue
		case straceOptionUnsafe:
			return true
		case straceOptionDeferredEnvironmentUnsafe:
			branchHazards.environment = true
		case straceOptionDeferredOutputUnsafe:
			branchHazards.output = true
		}
		if straceTailMutatesAccountEnvironment(words[action.consumed:], branchHazards, names, evaluation) {
			return true
		}
	}
	return false
}

func straceTailMutatesAccountEnvironment(
	words []*syntax.Word,
	hazards straceDeferredHazards,
	names map[string]struct{},
	evaluation *straceBoundaryEvaluation,
) bool {
	state := straceBoundaryState{remaining: len(words), hazards: hazards}
	if len(words) > 0 {
		state.first = words[0]
	}
	if result, found := evaluation.memo[state]; found {
		return result
	}
	if evaluation.memo == nil {
		evaluation.memo = make(map[straceBoundaryState]bool)
	}
	child, unsafe := unwrapStraceState(words, hazards, names, evaluation)
	result := unsafe
	if !unsafe {
		result = accountCommandWordsMutateEnvironment(child, names, evaluation)
	}
	evaluation.memo[state] = result
	return result
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
