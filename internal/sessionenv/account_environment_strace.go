package sessionenv

// strace is the wrapper that established the shared parser's grammar: it
// assigns executable meaning to one of its operands, so every word up to that
// operand must be understood before the child can be inspected. Its tables are
// stable arity proofs, not an exhaustive option model — a bare option absent
// from them keeps both the self-contained and separate-value boundaries alive,
// so an omitted newer option can cause a conservative refusal but cannot hide
// its child. -E/--env and -o/--output additionally carry environment-mutation
// and executable-output semantics that the shared evaluator defers and checks
// after option parsing.
var straceWrapperSpec = accountCommandWrapperSpec{
	// An absent short option produces both possible boundaries instead of
	// being guessed self-contained. 'f' stays proven argument-free to keep
	// clustered -fp usable when p's quoted operand is the last word.
	shortNoValue: "f",
	// Required-operand proofs. -E and -o are also required-operand options but
	// live in their semantic classes below; a bare unmodeled flag keeps both
	// boundaries rather than needing a spelling here.
	shortValue:            "abeIOpPsSuUX",
	shortTerminal:         "hV",
	shortEnvironment:      "E",
	shortExecutableOutput: "o",
	// A '=' ends a short-option cluster and begins an attached value. Nothing
	// after it can consume the following argv word.
	shortValueSeparator: true,
	longValue:           straceLongOptionPlainValues,
	longTerminal:        map[string]struct{}{"--help": {}, "--version": {}},
	longEnvironment:     map[string]struct{}{"--env": {}},
	longExecutableOutput: map[string]struct{}{
		"--output": {},
	},
	longAliases:       straceLongEquivalentAliases,
	longSelfContained: straceLongSelfContainedPrefixCollisions,
	// strace's option surface varies enough across releases that its tables are
	// proofs rather than a closed model, and its option words and operands can
	// end in one simple quoted scalar without moving the child boundary.
	unknownOptionBoundaries: true,
	quotedOptionSuffixes:    true,
	quotedScalarOperands:    true,
}

// These are stable required-value arity proofs, not the accepted option model.
// A proof narrows a token to the boundary after its operand. Every bare option
// absent from this set keeps both the self-contained and separate-value
// boundaries alive, so omitting a new strace option can cause a conservative
// refusal but cannot hide its child. The installed-strace oracle verifies that
// these proofs agree with the host binary.
//
// Environment and output stay in this arity table even though their semantics
// are checked after the shared value parser: they still consume exactly one
// operand.
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

// straceLongOptionPlainValues is the separate-value proof set minus the names
// whose operands carry environment or executable-output semantics, which the
// shared evaluator classifies by kind instead.
var straceLongOptionPlainValues = func() map[string]struct{} {
	values := make(map[string]struct{}, len(straceLongOptionsWithSeparateValue))
	for name := range straceLongOptionsWithSeparateValue {
		if name != "--env" && name != "--output" {
			values[name] = struct{}{}
		}
	}
	return values
}()

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

// classifyStraceLongOption keeps the installed-binary oracle's view of the
// shared resolver: the canonical family, whether the word reduces the parser
// (terminal), refuses it (conflict), or continues past it, and whether the
// spelling was exact.
func classifyStraceLongOption(option string) (string, wrapperResult, bool) {
	canonical, kind, exact := classifyWrapperLongOption(straceWrapperSpec, option)
	result := wrapperResultContinue
	switch kind {
	case wrapperKindTerminal:
		result = wrapperResultStops
	case wrapperKindUnsafe:
		result = wrapperResultUnsafe
	}
	return canonical, result, exact
}
