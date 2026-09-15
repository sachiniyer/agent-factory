package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// wrapperOptionKind classifies one option spelling in a wrapper's argv grammar.
type wrapperOptionKind uint8

const (
	wrapperKindUnknown wrapperOptionKind = iota
	wrapperKindNoValue
	wrapperKindValue
	wrapperKindOptionalAttachedValue
	wrapperKindTerminal
	wrapperKindEnvironmentValue
	wrapperKindExecutableOutput
	wrapperKindUnsafe
)

// wrapperSemantic names the side effect a semantic option's operand carries.
type wrapperSemantic uint8

const (
	wrapperSemanticEnvironment wrapperSemantic = iota
	wrapperSemanticOutput
)

type wrapperResult uint8

const (
	wrapperResultContinue wrapperResult = iota
	wrapperResultStops
	wrapperResultUnsafe
	wrapperResultDeferredEnvironmentUnsafe
	wrapperResultDeferredOutputUnsafe
)

type wrapperOptionToken struct {
	literalPrefix               string
	quotedOperand               bool
	quotedOperandMayConsumeNext bool
}

type wrapperOptionAction struct {
	consumed int
	result   wrapperResult
}

// wrapperDeferredHazards records semantic operands whose safety could not be
// proven at their own argv boundary. They are evaluated only against whatever
// the wrapper actually goes on to execute.
type wrapperDeferredHazards struct {
	environment bool
	output      bool
}

func (hazards wrapperDeferredHazards) affectChild() bool {
	return hazards.environment || hazards.output
}

type wrapperBoundaryEvaluation struct {
	memo map[wrapperBoundaryState]bool
}

// wrapperBoundaryState keys the ambiguous-suffix memo. An argv suffix is the
// same state whenever its first word is the same node, its length matches, and
// the deferred hazards carried into it agree.
type wrapperBoundaryState struct {
	first     *syntax.Word
	remaining int
	hazards   wrapperDeferredHazards
}

// accountCommandWrapperSpec is one wrapper's argv grammar as data. The parser
// below is the single closed evaluator: a recognized wrapper must reduce every
// option and operand before its child is recursively inspected, and an option
// the grammar cannot reduce either fails closed or keeps every child boundary
// it could have alive for checking, depending on unknownOptionBoundaries.
//
// The option tables are arity proofs, not an accepted-option inventory, for
// wrappers that set unknownOptionBoundaries; elsewhere they are the envelope
// and an unmodeled option refuses.
type accountCommandWrapperSpec struct {
	// Short-option classes, scanned inside clusters. Each flag belongs to at
	// most one class; an absent flag falls to the unknown policy.
	shortNoValue          string // proven argument-free; keep scanning
	shortValue            string // required operand: rest of word, else next word
	shortOptionalAttached string // optional operand: rest of word only
	shortTerminal         string // invocation exits or is process-only: no child
	shortEnvironment      string // operand mutates the executed child's environment
	shortExecutableOutput string // operand may name a command the wrapper runs
	shortUnsafe           string // unsafe regardless of operands

	longNoValue          map[string]struct{}
	longValue            map[string]struct{}
	longOptionalAttached map[string]struct{}
	longTerminal         map[string]struct{}
	longEnvironment      map[string]struct{}
	longExecutableOutput map[string]struct{}
	longUnsafe           map[string]struct{}
	longAliases          map[string]string
	longSelfContained    map[string]struct{}

	// unknownOptionBoundaries keeps both the self-contained and the
	// separate-value boundary for a bare option the tables do not model. Set it
	// where the tables are a cross-version proof set rather than an exhaustive
	// model, so a newer release's extra option cannot hide its child.
	unknownOptionBoundaries bool
	// quotedOptionSuffixes lets an option word end in one simple double-quoted
	// scalar parameter (for example -p"$PID" or --attach="$PID"), parsed
	// against the option's literal prefix.
	quotedOptionSuffixes bool
	// quotedScalarOperands lets a simple double-quoted scalar be an option
	// operand (for example -p "$PID"); it is exactly one argv word.
	quotedScalarOperands bool
	// shortValueSeparator ends a short-option cluster and begins an attached
	// value, so nothing after it can consume the following argv word.
	shortValueSeparator bool
	// unknownAttachedLongOpt accepts an unmodeled --name=value as
	// self-contained where the tool's grammar makes the attached form
	// complete (valgrind's --name=value surface).
	unknownAttachedLongOpt bool
	// negativeNumericOption accepts bare -N option words (nice -10).
	negativeNumericOption bool

	// leadingOperands counts fixed operands between options and the command
	// (timeout's duration, taskset's mask).
	leadingOperands int
	// optionalNumericLeadingOperand treats a non-negative decimal first
	// operand as a fixed operand and anything else as the command itself
	// (chrt's optional priority).
	optionalNumericLeadingOperand bool
	// implicitCommandUnsafe refuses a commandless invocation when the wrapper
	// resolves and starts an implicit shell from runtime account data
	// (unshare with no program).
	implicitCommandUnsafe bool
}

// accountCommandWrapperSpecs is the deliberately enumerated set of
// single-child executable wrappers reachable from an arbitrary process tab.
// Their argv grammars are data; unwrapAccountCommandWrapper is the one parser
// that proves where the child starts.
//
// perf is excluded because it is a subcommand dispatcher, not a single-child
// wrapper, and perf stat has independent --pre/--post shell commands in
// addition to its workload. xargs substitutes into and may execute a command
// repeatedly; gdb has both an inferior argv and debugger commands; sudo/doas
// apply privilege-specific environment policy. Those need dedicated analyzers
// rather than a dishonest entry in this shared table.
var accountCommandWrapperSpecs = map[string]accountCommandWrapperSpec{
	"nohup": {
		longTerminal: map[string]struct{}{"--help": {}, "--version": {}},
	},
	"nice": {
		shortValue:            "n",
		longValue:             map[string]struct{}{"--adjustment": {}},
		longTerminal:          map[string]struct{}{"--help": {}, "--version": {}},
		negativeNumericOption: true,
	},
	"timeout": {
		shortNoValue: "v",
		shortValue:   "ks",
		longNoValue: map[string]struct{}{
			"--foreground": {}, "--preserve-status": {}, "--verbose": {},
		},
		longValue:       map[string]struct{}{"--kill-after": {}, "--signal": {}},
		longTerminal:    map[string]struct{}{"--help": {}, "--version": {}},
		leadingOperands: 1,
	},
	"setsid": {
		shortNoValue:  "cfw",
		shortTerminal: "hV",
		longNoValue:   map[string]struct{}{"--ctty": {}, "--fork": {}, "--wait": {}},
		longTerminal:  map[string]struct{}{"--help": {}, "--version": {}},
	},
	"stdbuf": {
		shortValue:   "ioe",
		longValue:    map[string]struct{}{"--input": {}, "--output": {}, "--error": {}},
		longTerminal: map[string]struct{}{"--help": {}, "--version": {}},
	},
	"ionice": {
		shortNoValue:  "t",
		shortValue:    "cn",
		shortTerminal: "hVpPu",
		longNoValue:   map[string]struct{}{"--ignore": {}},
		longValue:     map[string]struct{}{"--class": {}, "--classdata": {}},
		longTerminal: map[string]struct{}{
			"--help": {}, "--version": {}, "--pid": {}, "--pgid": {}, "--uid": {},
		},
	},
	"taskset": {
		shortNoValue:    "ac",
		shortTerminal:   "hVp",
		longNoValue:     map[string]struct{}{"--all-tasks": {}, "--cpu-list": {}},
		longTerminal:    map[string]struct{}{"--help": {}, "--version": {}, "--pid": {}},
		leadingOperands: 1,
	},
	"strace": straceWrapperSpec,
	"ltrace": {
		shortNoValue:  "bcCfiLrStT",
		shortValue:    "aAdDeFlnopsuwx",
		shortTerminal: "hV",
		longNoValue:   map[string]struct{}{"--no-signals": {}, "--demangle": {}},
		longValue: map[string]struct{}{
			"--align": {}, "--debug": {}, "--config": {}, "--library": {},
			"--indent": {}, "--max-depth": {}, "--output": {}, "--where": {},
		},
		longTerminal: map[string]struct{}{"--help": {}, "--version": {}},
	},
	"valgrind": {
		shortNoValue:  "qvds",
		shortTerminal: "h",
		longNoValue:   map[string]struct{}{"--quiet": {}, "--verbose": {}},
		longTerminal: map[string]struct{}{
			"--help": {}, "--help-debug": {}, "--help-dyn-options": {}, "--version": {},
		},
		// Valgrind core and tool-specific value options use --name=value.
		// The manuals expose no option value as a second argv word and no
		// output option that executes its value, so an attached literal is a
		// complete option even when a selected tool owns the option name.
		unknownAttachedLongOpt: true,
	},
	"chrt": {
		shortNoValue:  "bdefiorRavGO",
		shortValue:    "TPDUX",
		shortTerminal: "mphV",
		longNoValue: map[string]struct{}{
			"--batch": {}, "--deadline": {}, "--ext": {}, "--fifo": {}, "--idle": {},
			"--other": {}, "--rr": {}, "--reclaim-grub": {}, "--deadline-overrun": {},
			"--reset-on-fork": {}, "--all-tasks": {}, "--verbose": {},
		},
		longValue: map[string]struct{}{
			"--sched-runtime": {}, "--sched-period": {}, "--sched-deadline": {},
			"--clamp-min": {}, "--clamp-max": {},
		},
		longTerminal:                  map[string]struct{}{"--max": {}, "--pid": {}, "--help": {}, "--version": {}},
		optionalNumericLeadingOperand: true,
	},
	"unshare": {
		shortNoValue:          "frc",
		shortValue:            "RwSG",
		shortOptionalAttached: "muinpUCT",
		shortTerminal:         "hV",
		shortUnsafe:           "l",
		longNoValue: map[string]struct{}{
			"--fork": {}, "--forward-signals": {}, "--map-root-user": {},
			"--map-current-user": {}, "--map-auto": {}, "--map-subids": {}, "--keep-caps": {},
		},
		longValue: map[string]struct{}{
			"--map-user": {}, "--map-group": {}, "--map-users": {}, "--map-groups": {},
			"--propagation": {}, "--owner": {}, "--setgroups": {}, "--root": {}, "--wd": {},
			"--setuid": {}, "--setgid": {}, "--monotonic": {}, "--boottime": {},
		},
		longOptionalAttached: map[string]struct{}{
			"--mount": {}, "--uts": {}, "--ipc": {}, "--net": {}, "--pid": {},
			"--user": {}, "--cgroup": {}, "--time": {}, "--kill-child": {},
			"--mount-proc": {}, "--mount-binfmt": {},
		},
		longTerminal: map[string]struct{}{"--help": {}, "--version": {}},
		longUnsafe:   map[string]struct{}{"--load-interp": {}, "--clear-env": {}, "--whitelist-env": {}},
		// Without a program, unshare resolves and starts an implicit shell from
		// runtime account data, so there is no statically inspectable child.
		implicitCommandUnsafe: true,
	},
}

// accountCommandWrapper resolves a literal executable word to its wrapper
// grammar, matching on the basename the way isAccountCommandName does.
func accountCommandWrapper(word *syntax.Word) (accountCommandWrapperSpec, bool) {
	command, literal := literalShellWord(word)
	if !literal {
		return accountCommandWrapperSpec{}, false
	}
	spec, ok := accountCommandWrapperSpecs[filepath.Base(command)]
	return spec, ok
}

// unwrapAccountCommandWrapper removes a wrapper's options and fixed operands
// so the command it executes can be recursively inspected. It is the one
// closed argv evaluator every modeled wrapper shares: each word must reduce
// through the selected grammar, and an unreduced or dynamic word cannot be
// mistaken for the child.
func unwrapAccountCommandWrapper(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) ([]*syntax.Word, bool) {
	evaluation := &wrapperBoundaryEvaluation{}
	return unwrapWrapperState(words, spec, wrapperDeferredHazards{}, names, evaluation)
}

func unwrapWrapperState(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	hazards wrapperDeferredHazards,
	names map[string]struct{},
	evaluation *wrapperBoundaryEvaluation,
) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		token, parsed := parseWrapperOptionToken(words[0], spec)
		if !parsed {
			return nil, true
		}
		option := token.literalPrefix
		if option == "--" {
			return wrapperChildAfterOperands(words[1:], spec, hazards)
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			return wrapperChildAfterOperands(words, spec, hazards)
		}
		if spec.negativeNumericOption && negativeNumericOption(option) {
			words = words[1:]
			continue
		}

		var actions []wrapperOptionAction
		if strings.HasPrefix(option, "--") {
			actions = parseWrapperLongOption(words, token, spec, names)
		} else {
			actions = parseWrapperShortOptions(words, token, spec, names)
		}
		if len(actions) != 1 {
			return nil, wrapperOptionActionsUnsafe(words, actions, spec, hazards, names, evaluation)
		}
		action := actions[0]
		if action.consumed > len(words) {
			// A required operand is absent, so option parsing exits before any
			// child or executable output target can be launched.
			return nil, false
		}
		switch action.result {
		case wrapperResultContinue:
			words = words[action.consumed:]
		case wrapperResultStops:
			// A terminal option exits during option parsing. Earlier semantic
			// hazards whose complete argv boundaries were known never take effect.
			return nil, false
		case wrapperResultUnsafe:
			return nil, true
		case wrapperResultDeferredEnvironmentUnsafe:
			hazards.environment = true
			words = words[action.consumed:]
		case wrapperResultDeferredOutputUnsafe:
			hazards.output = true
			words = words[action.consumed:]
		}
	}
	// Options consumed every word: there is no child for an environment option
	// to modify, while an executable output target still launches for attach
	// mode and therefore remains unsafe, and a wrapper that starts an implicit
	// shell without a program is likewise uninspectable.
	return nil, hazards.output || spec.implicitCommandUnsafe
}

// wrapperChildAfterOperands consumes a wrapper's fixed operands and returns
// the child argv. Reaching it means option parsing is done, so a pending
// hazard can only be discharged by the absence of anything left to run; a
// deferred output hazard survives even a missing child because attach mode
// still launches it.
func wrapperChildAfterOperands(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	hazards wrapperDeferredHazards,
) ([]*syntax.Word, bool) {
	leading := spec.leadingOperands
	if spec.optionalNumericLeadingOperand && len(words) > 0 {
		operand, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if nonNegativeDecimal(operand) {
			leading = 1
		}
	}
	if hazards.affectChild() {
		return nil, true
	}
	if len(words) <= leading {
		return nil, spec.implicitCommandUnsafe
	}
	for _, operand := range words[:leading] {
		if _, literal := literalShellWord(operand); !literal {
			return nil, true
		}
	}
	return words[leading:], false
}

func parseWrapperOptionToken(
	word *syntax.Word,
	spec accountCommandWrapperSpec,
) (wrapperOptionToken, bool) {
	if value, literal := literalShellWord(word); literal {
		return wrapperOptionToken{literalPrefix: value}, true
	}
	if !spec.quotedOptionSuffixes {
		return wrapperOptionToken{}, false
	}
	prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(word)
	if !dynamic {
		return wrapperOptionToken{}, false
	}
	consumes, mayConsumeNext := wrapperOptionConsumesQuotedSuffix(prefix, spec)
	if !consumes {
		return wrapperOptionToken{}, false
	}
	return wrapperOptionToken{
		literalPrefix:               prefix,
		quotedOperand:               true,
		quotedOperandMayConsumeNext: mayConsumeNext,
	}, true
}

// wrapperOptionConsumesQuotedSuffix reports whether an option word whose
// literal prefix ends in one simple quoted scalar can still own a stable argv
// boundary. A long option needs its literal '='; a short option's last flag
// must be value-taking, and it may consume the following argv word when the
// expansion is empty.
func wrapperOptionConsumesQuotedSuffix(
	prefix string,
	spec accountCommandWrapperSpec,
) (bool, bool) {
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
		kind := spec.shortOptionKind(flag)
		if kind == wrapperKindTerminal ||
			(spec.shortValueSeparator && flag == '=') {
			return false, false
		}
		switch kind {
		case wrapperKindValue, wrapperKindEnvironmentValue, wrapperKindExecutableOutput:
			// A literal suffix after the option makes the attached value
			// provably nonempty. Without one, the quoted expansion may vanish;
			// getopt then consumes the following argv word as the value instead.
			return true, idx+1 == len(prefix)
		}
	}
	return false, false
}

func parseWrapperLongOption(
	words []*syntax.Word,
	token wrapperOptionToken,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) []wrapperOptionAction {
	value := token.literalPrefix
	option, attachedValue, attached := strings.Cut(value, "=")
	_, kind, exact := classifyWrapperLongOption(spec, option)
	switch kind {
	case wrapperKindTerminal:
		// A terminal option never launches the trailing command. An attached
		// value is either accepted by that tool version or rejected before
		// launch, so neither spelling needs a child-boundary decision.
		actions := []wrapperOptionAction{{result: wrapperResultStops}}
		if !exact && spec.unknownOptionBoundaries {
			// The abbreviated name could also be a self-contained option on a
			// release whose table differs, so keep that boundary alive.
			actions = append(actions, wrapperOptionAction{consumed: 1, result: wrapperResultContinue})
		}
		return actions
	case wrapperKindUnsafe:
		return []wrapperOptionAction{{result: wrapperResultUnsafe}}
	case wrapperKindUnknown:
		if attached {
			if spec.unknownOptionBoundaries || spec.unknownAttachedLongOpt {
				return []wrapperOptionAction{{consumed: 1, result: wrapperResultContinue}}
			}
			return []wrapperOptionAction{{result: wrapperResultUnsafe}}
		}
		if spec.unknownOptionBoundaries {
			return wrapperPossibleValueBoundaries()
		}
		return []wrapperOptionAction{{result: wrapperResultUnsafe}}
	}

	var action wrapperOptionAction
	switch kind {
	case wrapperKindEnvironmentValue, wrapperKindExecutableOutput:
		action = parseWrapperSemanticOptionValue(words, token, attachedValue, attached, kind, names)
	case wrapperKindValue:
		if token.quotedOperand {
			action = wrapperOptionAction{consumed: 1, result: wrapperResultContinue}
		} else {
			var ok bool
			action, ok = wrapperValueAction(words, spec, attachedValue, attached)
			if !ok {
				return []wrapperOptionAction{{result: wrapperResultUnsafe}}
			}
		}
	case wrapperKindNoValue, wrapperKindOptionalAttachedValue:
		// A no-argument name with an attached value is rejected by getopt
		// before launch, and an optional-attached value is complete either
		// way, so both consume exactly this argv word.
		action = wrapperOptionAction{consumed: 1, result: wrapperResultContinue}
	}
	return []wrapperOptionAction{action}
}

// classifyWrapperLongOption resolves a long-option name to its grammar family
// and kind. Exact names win first — getopt_long gives them precedence over
// abbreviations — then every modeled name the word is a prefix of is grouped
// by family and kind. A unique match is that family's grammar; matches across
// inequivalent families or kinds fail closed because which semantics apply
// cannot be proven, and no match leaves the unknown policy to the caller.
func classifyWrapperLongOption(
	spec accountCommandWrapperSpec,
	option string,
) (string, wrapperOptionKind, bool) {
	if kind, found := spec.longOptionKind(option); found {
		return spec.longOptionFamily(option), kind, true
	}
	if _, selfContained := spec.longSelfContained[option]; selfContained {
		return "", wrapperKindNoValue, true
	}

	matchFamily := ""
	matchKind := wrapperKindUnknown
	conflict := false
	addMatch := func(family string, kind wrapperOptionKind) {
		if matchFamily == "" {
			matchFamily = family
			matchKind = kind
			return
		}
		if matchFamily != family || matchKind != kind {
			conflict = true
		}
	}
	spec.eachLongOption(func(name string, kind wrapperOptionKind) {
		if strings.HasPrefix(name, option) {
			addMatch(spec.longOptionFamily(name), kind)
		}
	})
	if conflict {
		return "", wrapperKindUnsafe, false
	}
	if matchFamily == "" {
		return "", wrapperKindUnknown, false
	}
	return matchFamily, matchKind, false
}

func parseWrapperShortOptions(
	words []*syntax.Word,
	token wrapperOptionToken,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) []wrapperOptionAction {
	return parseWrapperShortOptionAt(words, token, spec, names, 1)
}

func parseWrapperShortOptionAt(
	words []*syntax.Word,
	token wrapperOptionToken,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
	idx int,
) []wrapperOptionAction {
	value := token.literalPrefix
	if idx >= len(value) {
		return []wrapperOptionAction{{consumed: 1, result: wrapperResultContinue}}
	}
	flag := value[idx]
	if spec.shortValueSeparator && flag == '=' {
		return []wrapperOptionAction{{consumed: 1, result: wrapperResultContinue}}
	}
	switch kind := spec.shortOptionKind(flag); kind {
	case wrapperKindNoValue:
		return parseWrapperShortOptionAt(words, token, spec, names, idx+1)
	case wrapperKindOptionalAttachedValue:
		return []wrapperOptionAction{{consumed: 1, result: wrapperResultContinue}}
	case wrapperKindTerminal:
		return []wrapperOptionAction{{result: wrapperResultStops}}
	case wrapperKindEnvironmentValue, wrapperKindExecutableOutput:
		attached := idx+1 < len(value)
		attachedValue := ""
		if attached {
			attachedValue = value[idx+1:]
		}
		return []wrapperOptionAction{parseWrapperSemanticOptionValue(
			words, token, attachedValue, attached, kind, names,
		)}
	case wrapperKindValue:
		attached := idx+1 < len(value)
		attachedValue := ""
		if attached {
			attachedValue = value[idx+1:]
		}
		if token.quotedOperand {
			if token.quotedOperandMayConsumeNext {
				return wrapperPossibleValueBoundaries()
			}
			return []wrapperOptionAction{{consumed: 1, result: wrapperResultContinue}}
		}
		action, ok := wrapperValueAction(words, spec, attachedValue, attached)
		if !ok {
			return []wrapperOptionAction{{result: wrapperResultUnsafe}}
		}
		return []wrapperOptionAction{action}
	case wrapperKindUnsafe:
		return []wrapperOptionAction{{result: wrapperResultUnsafe}}
	default:
		if !spec.unknownOptionBoundaries {
			return []wrapperOptionAction{{result: wrapperResultUnsafe}}
		}
		// An unmodeled short flag is never assigned a guessed arity. One branch
		// treats it as argument-free and keeps parsing the cluster; the other
		// treats the rest of this word, or the following word when it is last,
		// as its operand. Stable arity proofs above only remove impossible
		// branches; omissions therefore fail closed rather than hiding a child.
		actions := parseWrapperShortOptionAt(words, token, spec, names, idx+1)
		consumed := 1
		if idx+1 == len(value) {
			consumed = 2
		}
		return appendUniqueWrapperAction(actions, wrapperOptionAction{
			consumed: consumed,
			result:   wrapperResultContinue,
		})
	}
}

func wrapperPossibleValueBoundaries() []wrapperOptionAction {
	return []wrapperOptionAction{
		{consumed: 1, result: wrapperResultContinue},
		{consumed: 2, result: wrapperResultContinue},
	}
}

func appendUniqueWrapperAction(
	actions []wrapperOptionAction,
	action wrapperOptionAction,
) []wrapperOptionAction {
	for _, existing := range actions {
		if existing == action {
			return actions
		}
	}
	return append(actions, action)
}

// wrapperValueAction reduces a required option operand. A missing operand
// keeps the boundary past argv so the caller's exhausted-input branch can
// accept it — option parsing exits before a child launches.
func wrapperValueAction(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	attachedValue string,
	attached bool,
) (wrapperOptionAction, bool) {
	if spec.quotedScalarOperands {
		_, consumed, ok := wrapperOperandValueAllowQuotedScalar(words, attachedValue, attached)
		return wrapperOptionAction{consumed: consumed, result: wrapperResultContinue}, ok
	}
	_, consumed, ok := wrapperOperandValue(words, attachedValue, attached)
	if !ok && !attached && len(words) < 2 {
		return wrapperOptionAction{consumed: 2, result: wrapperResultContinue}, true
	}
	return wrapperOptionAction{consumed: consumed, result: wrapperResultContinue}, ok
}

func wrapperOperandValue(
	words []*syntax.Word,
	attachedValue string,
	attached bool,
) (string, int, bool) {
	if attached {
		return attachedValue, 1, attachedValue != ""
	}
	if len(words) < 2 {
		return "", 0, false
	}
	value, literal := literalShellWord(words[1])
	return value, 2, literal
}

func wrapperOperandValueAllowQuotedScalar(
	words []*syntax.Word,
	attachedValue string,
	attached bool,
) (string, int, bool) {
	operand, consumed, ok := wrapperOperandValue(words, attachedValue, attached)
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

func parseWrapperSemanticOptionValue(
	words []*syntax.Word,
	token wrapperOptionToken,
	attachedValue string,
	attached bool,
	kind wrapperOptionKind,
	names map[string]struct{},
) wrapperOptionAction {
	semantic := wrapperSemanticEnvironment
	if kind == wrapperKindExecutableOutput {
		semantic = wrapperSemanticOutput
	}
	if token.quotedOperand {
		if token.quotedOperandMayConsumeNext {
			return wrapperOptionAction{result: wrapperResultUnsafe}
		}
		return wrapperOptionAction{
			consumed: 1,
			result:   wrapperDynamicSemanticResult(semantic, attachedValue, names),
		}
	}
	operand, consumed, literal := wrapperOperandValue(words, attachedValue, attached)
	if literal {
		return wrapperOptionAction{
			consumed: consumed,
			result:   wrapperLiteralSemanticResult(semantic, operand, names),
		}
	}
	if len(words) < 2 {
		return wrapperOptionAction{consumed: 2, result: wrapperResultContinue}
	}
	if attached {
		return wrapperOptionAction{result: wrapperResultUnsafe}
	}
	prefix, dynamic := literalPrefixBeforeSimpleQuotedParameter(words[1])
	if !dynamic {
		return wrapperOptionAction{result: wrapperResultUnsafe}
	}
	return wrapperOptionAction{
		consumed: 2,
		result:   wrapperDynamicSemanticResult(semantic, prefix, names),
	}
}

func wrapperLiteralSemanticResult(
	semantic wrapperSemantic,
	operand string,
	names map[string]struct{},
) wrapperResult {
	switch semantic {
	case wrapperSemanticEnvironment:
		if wrapperEnvironmentMutationUnsafe(operand, names) {
			return wrapperResultDeferredEnvironmentUnsafe
		}
	case wrapperSemanticOutput:
		if wrapperOutputTargetUnsafe(operand) {
			return wrapperResultDeferredOutputUnsafe
		}
	}
	return wrapperResultContinue
}

func wrapperDynamicSemanticResult(
	semantic wrapperSemantic,
	literalPrefix string,
	names map[string]struct{},
) wrapperResult {
	// Accepted dynamic semantic values are deliberately bounded: environment
	// values need a complete literal NAME= prefix, and output targets need a
	// literal first byte. Anything else is deferred rather than refused here:
	// an unprovable value is a hazard only when a child (or, for an output
	// target, an attach) actually launches, so a later terminal option or a
	// childless attach must still clear it, exactly like the literal hazard
	// forms. A surviving deferred hazard still fails closed on any executed
	// child, keeping forms such as -E "$SPEC" and --output="$OUT" refused.
	switch semantic {
	case wrapperSemanticEnvironment:
		name, _, fixedName := strings.Cut(literalPrefix, "=")
		if !fixedName || !validName(name) {
			return wrapperResultDeferredEnvironmentUnsafe
		}
		if accountEnvironmentNameDenied(name, names) {
			return wrapperResultDeferredEnvironmentUnsafe
		}
	case wrapperSemanticOutput:
		if literalPrefix == "" {
			return wrapperResultDeferredOutputUnsafe
		}
		if wrapperOutputTargetUnsafe(literalPrefix) {
			return wrapperResultDeferredOutputUnsafe
		}
	}
	return wrapperResultContinue
}

// wrapperOptionActionsUnsafe reports whether every boundary the ambiguous
// token could produce launches a mutating child or leaves a live hazard. Each
// action names one candidate boundary; the token is safe only when all of them
// are, so the first mutating tail ends the search.
func wrapperOptionActionsUnsafe(
	words []*syntax.Word,
	actions []wrapperOptionAction,
	spec accountCommandWrapperSpec,
	hazards wrapperDeferredHazards,
	names map[string]struct{},
	evaluation *wrapperBoundaryEvaluation,
) bool {
	for _, action := range actions {
		if action.consumed > len(words) {
			continue
		}
		branchHazards := hazards
		switch action.result {
		case wrapperResultStops:
			continue
		case wrapperResultUnsafe:
			return true
		case wrapperResultDeferredEnvironmentUnsafe:
			branchHazards.environment = true
		case wrapperResultDeferredOutputUnsafe:
			branchHazards.output = true
		}
		if wrapperTailMutatesAccountEnvironment(words[action.consumed:], spec, branchHazards, names, evaluation) {
			return true
		}
	}
	return false
}

// wrapperTailMutatesAccountEnvironment evaluates one ambiguous-boundary
// suffix. States are memoized so a run of unmodeled options cannot rederive
// the same remaining argv exponentially.
func wrapperTailMutatesAccountEnvironment(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	hazards wrapperDeferredHazards,
	names map[string]struct{},
	evaluation *wrapperBoundaryEvaluation,
) bool {
	state := wrapperBoundaryState{remaining: len(words), hazards: hazards}
	if len(words) > 0 {
		state.first = words[0]
	}
	if result, found := evaluation.memo[state]; found {
		return result
	}
	if evaluation.memo == nil {
		evaluation.memo = make(map[wrapperBoundaryState]bool)
	}
	child, unsafe := unwrapWrapperState(words, spec, hazards, names, evaluation)
	result := unsafe
	if !unsafe {
		result = accountCommandWordsMutateEnvironment(child, names)
	}
	evaluation.memo[state] = result
	return result
}

func wrapperEnvironmentMutationUnsafe(operand string, names map[string]struct{}) bool {
	name, _, _ := strings.Cut(operand, "=")
	if !validName(name) {
		return true
	}
	return accountEnvironmentNameDenied(name, names)
}

func wrapperOutputTargetUnsafe(operand string) bool {
	return strings.HasPrefix(operand, "|") || strings.HasPrefix(operand, "!")
}

func (s accountCommandWrapperSpec) shortOptionKind(flag byte) wrapperOptionKind {
	switch {
	case strings.ContainsRune(s.shortNoValue, rune(flag)):
		return wrapperKindNoValue
	case strings.ContainsRune(s.shortValue, rune(flag)):
		return wrapperKindValue
	case strings.ContainsRune(s.shortOptionalAttached, rune(flag)):
		return wrapperKindOptionalAttachedValue
	case strings.ContainsRune(s.shortTerminal, rune(flag)):
		return wrapperKindTerminal
	case strings.ContainsRune(s.shortEnvironment, rune(flag)):
		return wrapperKindEnvironmentValue
	case strings.ContainsRune(s.shortExecutableOutput, rune(flag)):
		return wrapperKindExecutableOutput
	case strings.ContainsRune(s.shortUnsafe, rune(flag)):
		return wrapperKindUnsafe
	default:
		return wrapperKindUnknown
	}
}

func (s accountCommandWrapperSpec) longOptionKind(name string) (wrapperOptionKind, bool) {
	for _, set := range []struct {
		names map[string]struct{}
		kind  wrapperOptionKind
	}{
		{s.longNoValue, wrapperKindNoValue},
		{s.longValue, wrapperKindValue},
		{s.longOptionalAttached, wrapperKindOptionalAttachedValue},
		{s.longTerminal, wrapperKindTerminal},
		{s.longEnvironment, wrapperKindEnvironmentValue},
		{s.longExecutableOutput, wrapperKindExecutableOutput},
		{s.longUnsafe, wrapperKindUnsafe},
	} {
		if _, found := set.names[name]; found {
			return set.kind, true
		}
	}
	return wrapperKindUnknown, false
}

// eachLongOption visits every modeled long name once for abbreviation
// matching. Self-contained collision names are deliberately absent: they exist
// to give their exact spelling precedence, not to act as prefix candidates.
func (s accountCommandWrapperSpec) eachLongOption(visit func(name string, kind wrapperOptionKind)) {
	for _, set := range []struct {
		names map[string]struct{}
		kind  wrapperOptionKind
	}{
		{s.longNoValue, wrapperKindNoValue},
		{s.longValue, wrapperKindValue},
		{s.longOptionalAttached, wrapperKindOptionalAttachedValue},
		{s.longTerminal, wrapperKindTerminal},
		{s.longEnvironment, wrapperKindEnvironmentValue},
		{s.longExecutableOutput, wrapperKindExecutableOutput},
		{s.longUnsafe, wrapperKindUnsafe},
	} {
		for name := range set.names {
			visit(name, set.kind)
		}
	}
}

func (s accountCommandWrapperSpec) longOptionFamily(name string) string {
	if family, alias := s.longAliases[name]; alias {
		return family
	}
	return name
}

func negativeNumericOption(option string) bool {
	return len(option) > 1 && option[0] == '-' && strings.Trim(option[1:], "0123456789") == ""
}

func nonNegativeDecimal(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}
