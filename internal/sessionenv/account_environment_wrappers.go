package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type wrapperParseResult uint8

const (
	wrapperParseContinue wrapperParseResult = iota
	wrapperParseStops
	wrapperParseUnsafe
)

type wrapperOptionKind uint8

const (
	wrapperOptionUnknown wrapperOptionKind = iota
	wrapperOptionNoValue
	wrapperOptionValue
	wrapperOptionOptionalAttachedValue
	wrapperOptionTerminal
	wrapperOptionEnvironmentValue
	wrapperOptionExecutableOutput
	wrapperOptionUnsafe
)

type accountCommandWrapperSpec struct {
	shortNoValue          string
	shortValue            string
	shortOptionalValue    string
	shortTerminal         string
	shortEnvironmentValue string
	shortExecutableOutput string
	shortUnsafe           string

	longNoValue          []string
	longValue            []string
	longOptionalValue    []string
	longTerminal         []string
	longEnvironmentValue []string
	longExecutableOutput []string
	longUnsafe           []string

	leadingOperands               int
	optionalNumericLeadingOperand bool
	implicitCommandUnsafe         bool
	allowNegativeNumericOption    bool
	allowUnknownAttachedLongOpt   bool
}

// accountCommandWrapperSpecs is the deliberately enumerated set of
// single-child executable wrappers reachable from an arbitrary process tab.
// Their argv grammars are data; unwrapAccountCommandWrapper is the one parser
// that proves where the child starts. An option or word the selected grammar
// cannot reduce fails closed rather than being mistaken for the child.
//
// perf is excluded because it is a subcommand dispatcher, not a single-child
// wrapper, and perf stat has independent --pre/--post shell commands in
// addition to its workload. xargs substitutes into and may execute a command
// repeatedly; gdb has both an inferior argv and debugger commands; sudo/doas
// apply privilege-specific environment policy. Those need dedicated analyzers
// rather than a dishonest entry in this shared table.
var accountCommandWrapperSpecs = map[string]accountCommandWrapperSpec{
	"nohup": {
		longTerminal: []string{"--help", "--version"},
	},
	"nice": {
		shortValue:                 "n",
		longValue:                  []string{"--adjustment"},
		longTerminal:               []string{"--help", "--version"},
		allowNegativeNumericOption: true,
	},
	"timeout": {
		shortNoValue:    "v",
		shortValue:      "ks",
		longNoValue:     []string{"--foreground", "--preserve-status", "--verbose"},
		longValue:       []string{"--kill-after", "--signal"},
		longTerminal:    []string{"--help", "--version"},
		leadingOperands: 1,
	},
	"setsid": {
		shortNoValue:  "cfw",
		shortTerminal: "hV",
		longNoValue:   []string{"--ctty", "--fork", "--wait"},
		longTerminal:  []string{"--help", "--version"},
	},
	"stdbuf": {
		shortValue:   "ioe",
		longValue:    []string{"--input", "--output", "--error"},
		longTerminal: []string{"--help", "--version"},
	},
	"ionice": {
		shortNoValue:  "t",
		shortValue:    "cn",
		shortTerminal: "hVpPu",
		longNoValue:   []string{"--ignore"},
		longValue:     []string{"--class", "--classdata"},
		longTerminal:  []string{"--help", "--version", "--pid", "--pgid", "--uid"},
	},
	"taskset": {
		shortNoValue:    "ac",
		shortTerminal:   "hVp",
		longNoValue:     []string{"--all-tasks", "--cpu-list"},
		longTerminal:    []string{"--help", "--version", "--pid"},
		leadingOperands: 1,
	},
	"strace": {
		shortNoValue:          "ACDcdfiknqrtTvwxyYzZ",
		shortValue:            "abeIOpPsSuUX",
		shortTerminal:         "hV",
		shortEnvironmentValue: "E",
		shortExecutableOutput: "o",
		longNoValue: []string{
			"--debug", "--follow-forks", "--instruction-pointer", "--kill-on-exit",
			"--no-abbrev", "--output-append-mode", "--output-separately", "--seccomp-bpf",
			"--successful-only", "--failed-only", "--summary", "--summary-only",
			"--summary-wall-clock", "--syscall-number",
		},
		longValue: []string{
			"--abbrev", "--argv0", "--attach", "--columns", "--const-print-style",
			"--decode-pids", "--detach-on", "--fault", "--inject", "--interruptible",
			"--raw", "--read", "--signal", "--stack-trace-frame-limit", "--status",
			"--string-limit", "--summary-columns", "--summary-sort-by",
			"--summary-syscall-overhead", "--syscall-limit", "--trace", "--trace-fds",
			"--trace-path", "--user", "--verbose", "--write",
		},
		longOptionalValue: []string{
			"--absolute-timestamps", "--daemonize", "--decode-fds", "--quiet",
			"--relative-timestamps", "--stack-trace", "--strings-in-hex", "--syscall-times",
			"--tips",
		},
		longTerminal:         []string{"--help", "--version"},
		longEnvironmentValue: []string{"--env"},
		longExecutableOutput: []string{"--output"},
	},
	"ltrace": {
		shortNoValue:  "bcCfiLrStT",
		shortValue:    "aAdDeFlnopsuwx",
		shortTerminal: "hV",
		longNoValue:   []string{"--no-signals", "--demangle"},
		longValue: []string{
			"--align", "--debug", "--config", "--library", "--indent", "--max-depth", "--output", "--where",
		},
		longTerminal: []string{"--help", "--version"},
	},
	"valgrind": {
		shortNoValue:  "qvds",
		shortTerminal: "h",
		longNoValue:   []string{"--quiet", "--verbose"},
		longTerminal:  []string{"--help", "--help-debug", "--help-dyn-options", "--version"},
		// Valgrind core and tool-specific value options use --name=value.
		// The manuals expose no option value as a second argv word and no
		// output option that executes its value, so an attached literal is a
		// complete option even when a selected tool owns the option name.
		allowUnknownAttachedLongOpt: true,
	},
	"chrt": {
		shortNoValue:  "bdefiorRavGO",
		shortValue:    "TPDUX",
		shortTerminal: "mphV",
		longNoValue: []string{
			"--batch", "--deadline", "--ext", "--fifo", "--idle", "--other", "--rr",
			"--reclaim-grub", "--deadline-overrun", "--reset-on-fork", "--all-tasks", "--verbose",
		},
		longValue: []string{
			"--sched-runtime", "--sched-period", "--sched-deadline", "--clamp-min", "--clamp-max",
		},
		longTerminal:                  []string{"--max", "--pid", "--help", "--version"},
		optionalNumericLeadingOperand: true,
	},
	"unshare": {
		shortNoValue:       "frc",
		shortValue:         "RwSG",
		shortOptionalValue: "muinpUCT",
		shortTerminal:      "hV",
		shortUnsafe:        "l",
		longNoValue: []string{
			"--fork", "--forward-signals", "--map-root-user", "--map-current-user", "--map-auto",
			"--map-subids", "--keep-caps",
		},
		longValue: []string{
			"--map-user", "--map-group", "--map-users", "--map-groups", "--propagation",
			"--owner", "--setgroups", "--root", "--wd", "--setuid", "--setgid", "--monotonic", "--boottime",
		},
		longOptionalValue: []string{
			"--mount", "--uts", "--ipc", "--net", "--pid", "--user", "--cgroup", "--time",
			"--kill-child", "--mount-proc", "--mount-binfmt",
		},
		longTerminal: []string{"--help", "--version"},
		longUnsafe:   []string{"--load-interp", "--clear-env", "--whitelist-env"},
		// Without a program, unshare resolves and starts an implicit shell from
		// runtime account data, so there is no statically inspectable child.
		implicitCommandUnsafe: true,
	},
}

func accountCommandWrapper(word *syntax.Word) (accountCommandWrapperSpec, bool) {
	command, literal := literalShellWord(word)
	if !literal {
		return accountCommandWrapperSpec{}, false
	}
	spec, ok := accountCommandWrapperSpecs[filepath.Base(command)]
	return spec, ok
}

func unwrapAccountCommandWrapper(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			words = words[1:]
			break
		}
		if option == "-" || !strings.HasPrefix(option, "-") {
			break
		}
		if spec.allowNegativeNumericOption && negativeNumericOption(option) {
			words = words[1:]
			continue
		}

		consumed, result := parseAccountWrapperOption(words, spec, names)
		switch result {
		case wrapperParseContinue:
			words = words[consumed:]
		case wrapperParseStops:
			return nil, false
		case wrapperParseUnsafe:
			return nil, true
		}
	}

	leadingOperands := spec.leadingOperands
	if spec.optionalNumericLeadingOperand && len(words) > 0 {
		operand, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if nonNegativeDecimal(operand) {
			leadingOperands = 1
		}
	}
	if len(words) <= leadingOperands {
		return nil, spec.implicitCommandUnsafe
	}
	for _, operand := range words[:leadingOperands] {
		if _, literal := literalShellWord(operand); !literal {
			return nil, true
		}
	}
	return words[leadingOperands:], false
}

func parseAccountWrapperOption(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) (int, wrapperParseResult) {
	option, _ := literalShellWord(words[0])
	if strings.HasPrefix(option, "--") {
		return parseAccountWrapperLongOption(words, spec, names)
	}
	return parseAccountWrapperShortOptions(words, spec, names)
}

func parseAccountWrapperLongOption(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) (int, wrapperParseResult) {
	value, _ := literalShellWord(words[0])
	option, attachedValue, attached := strings.Cut(value, "=")
	kind := spec.longOptionKind(option)
	if kind == wrapperOptionUnknown && spec.allowUnknownAttachedLongOpt && attached {
		return 1, wrapperParseContinue
	}
	return consumeAccountWrapperOption(words, kind, attachedValue, attached, names)
}

func parseAccountWrapperShortOptions(
	words []*syntax.Word,
	spec accountCommandWrapperSpec,
	names map[string]struct{},
) (int, wrapperParseResult) {
	value, _ := literalShellWord(words[0])
	for idx := 1; idx < len(value); idx++ {
		kind := spec.shortOptionKind(value[idx])
		switch kind {
		case wrapperOptionNoValue:
			continue
		case wrapperOptionOptionalAttachedValue:
			return 1, wrapperParseContinue
		case wrapperOptionTerminal:
			return 0, wrapperParseStops
		case wrapperOptionValue, wrapperOptionEnvironmentValue, wrapperOptionExecutableOutput:
			attached := idx+1 < len(value)
			attachedValue := ""
			if attached {
				attachedValue = value[idx+1:]
			}
			return consumeAccountWrapperOption(words, kind, attachedValue, attached, names)
		case wrapperOptionUnsafe:
			return 0, wrapperParseUnsafe
		default:
			return 0, wrapperParseUnsafe
		}
	}
	return 1, wrapperParseContinue
}

func consumeAccountWrapperOption(
	words []*syntax.Word,
	kind wrapperOptionKind,
	attachedValue string,
	attached bool,
	names map[string]struct{},
) (int, wrapperParseResult) {
	switch kind {
	case wrapperOptionNoValue:
		if attached {
			return 0, wrapperParseUnsafe
		}
		return 1, wrapperParseContinue
	case wrapperOptionOptionalAttachedValue:
		return 1, wrapperParseContinue
	case wrapperOptionTerminal:
		if attached {
			return 0, wrapperParseUnsafe
		}
		return 0, wrapperParseStops
	case wrapperOptionValue, wrapperOptionEnvironmentValue, wrapperOptionExecutableOutput:
		operand, consumed, ok := accountWrapperOptionValue(words, attachedValue, attached)
		if !ok {
			return 0, wrapperParseUnsafe
		}
		if kind == wrapperOptionEnvironmentValue && straceEnvironmentMutationUnsafe(operand, names) {
			return 0, wrapperParseUnsafe
		}
		if kind == wrapperOptionExecutableOutput && straceOutputTargetUnsafe(operand) {
			return 0, wrapperParseUnsafe
		}
		return consumed, wrapperParseContinue
	case wrapperOptionUnsafe:
		return 0, wrapperParseUnsafe
	default:
		return 0, wrapperParseUnsafe
	}
}

func accountWrapperOptionValue(words []*syntax.Word, attachedValue string, attached bool) (string, int, bool) {
	if attached {
		return attachedValue, 1, attachedValue != ""
	}
	if len(words) < 2 {
		return "", 0, false
	}
	value, literal := literalShellWord(words[1])
	return value, 2, literal
}

func (s accountCommandWrapperSpec) shortOptionKind(option byte) wrapperOptionKind {
	switch {
	case strings.ContainsRune(s.shortNoValue, rune(option)):
		return wrapperOptionNoValue
	case strings.ContainsRune(s.shortValue, rune(option)):
		return wrapperOptionValue
	case strings.ContainsRune(s.shortOptionalValue, rune(option)):
		return wrapperOptionOptionalAttachedValue
	case strings.ContainsRune(s.shortTerminal, rune(option)):
		return wrapperOptionTerminal
	case strings.ContainsRune(s.shortEnvironmentValue, rune(option)):
		return wrapperOptionEnvironmentValue
	case strings.ContainsRune(s.shortExecutableOutput, rune(option)):
		return wrapperOptionExecutableOutput
	case strings.ContainsRune(s.shortUnsafe, rune(option)):
		return wrapperOptionUnsafe
	default:
		return wrapperOptionUnknown
	}
}

func (s accountCommandWrapperSpec) longOptionKind(option string) wrapperOptionKind {
	switch {
	case slices.Contains(s.longNoValue, option):
		return wrapperOptionNoValue
	case slices.Contains(s.longValue, option):
		return wrapperOptionValue
	case slices.Contains(s.longOptionalValue, option):
		return wrapperOptionOptionalAttachedValue
	case slices.Contains(s.longTerminal, option):
		return wrapperOptionTerminal
	case slices.Contains(s.longEnvironmentValue, option):
		return wrapperOptionEnvironmentValue
	case slices.Contains(s.longExecutableOutput, option):
		return wrapperOptionExecutableOutput
	case slices.Contains(s.longUnsafe, option):
		return wrapperOptionUnsafe
	default:
		return wrapperOptionUnknown
	}
}

func negativeNumericOption(option string) bool {
	return len(option) > 1 && option[0] == '-' && strings.Trim(option[1:], "0123456789") == ""
}

func nonNegativeDecimal(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}
