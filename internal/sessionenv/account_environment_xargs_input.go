package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// xargs input — an appended item, or the line a -I/-i/--replace marker is
// replaced with — must never be able to become a program word, an env operand,
// or an option value that a wrapper feeds to env (#4978). unwrapXargs's env
// scan covers the env operand region; this file follows the chain xargs
// actually runs (modeled wrappers, env, strace) to the positions that scan
// cannot see:
//
//   - A chain that ends without naming a command hands appended items the
//     program slot: `xargs nohup` fed `env CODEX_HOME=/x codex` runs
//     `nohup env CODEX_HOME=/x codex`, and `xargs env A=1 nohup` does the same
//     one link further in. A marker at any link's head is that program:
//     `xargs -I{} nohup {}`.
//   - strace's -E/--env puts a variable into the traced child's environment,
//     so input that can reach strace's option region — an appended item when
//     strace names no command yet, or a marker in or as an option word — can
//     override the account root: `xargs -I{} strace --env={} codex`.
//   - A nested xargs's -I and --process-slot-var are its own env-reaching
//     options, so an outer marker in its option region is refused.
//   - A link the walk cannot parse is refused when input reaches it. Its
//     binary may still accept it: getopt_long takes unambiguous prefixes, so
//     `nice --adj` and `xargs --proc=` are real options the modeled wrappers
//     reject. Appended items always reach it (they land at its end), and a
//     marker reaches it when the link's words carry one.
//
// An unmodeled program's argv is outside this model: nothing here knows which
// unmodeled programs exec their arguments, and refusing input after every one
// of them would refuse `xargs rm`. Input there keeps the treatment the walk
// gives an unknown "$x" in the same place.
//
// Every scan here is memoized per suffix position, so the walk stays linear
// in the command's word count (#4966).

// xargsInputKey memoizes xargsInputReaches by chain suffix and input mode.
type xargsInputKey struct {
	word         *syntax.Word
	substituting bool
	marker       string
}

// xargsNestedKey memoizes the nested-xargs option-region scan by position,
// outer marker, and pending-value state.
type xargsNestedKey struct {
	word    *syntax.Word
	marker  string
	pending xargsNestedPending
}

type xargsNestedPending uint8

const (
	xargsNestedNone xargsNestedPending = iota
	xargsNestedValue
	xargsNestedDangerousValue
)

// xargsNestedResult is the scan's answer from a position: whether the marker
// reaches an env-reaching option before the region ends, and the length of
// the suffix left after the region.
type xargsNestedResult struct {
	carries bool
	rest    int
}

// xargsChild peels the modeled wrappers off the argv xargs execs and refuses
// it when xargs input can reach env through the chain. The peeled tail is
// returned so the caller does not peel it again.
func xargsChild(words []*syntax.Word, substituting bool, marker string, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	if substituting && memo.xargsMarkerInNestedXargsOptions(words, marker) {
		return nil, true
	}
	tail, unsafe := unwrapAccountCommand(words, names, memo)
	if unsafe || xargsInputReaches(tail, substituting, marker, names, memo) {
		return nil, true
	}
	return tail, false
}

// xargsInputReaches judges a peeled chain: tail has had its modeled wrappers
// removed, so its head is the program the chain runs, or it is empty.
func xargsInputReaches(tail []*syntax.Word, substituting bool, marker string, names map[string]struct{}, memo operandTailMemo) bool {
	if len(tail) == 0 {
		// No command: appended items become the program. A substituted
		// command with no argv past its wrappers has nothing appended.
		return !substituting
	}
	key := xargsInputKey{word: tail[0], substituting: substituting, marker: marker}
	if answer, seen := memo.xargsInput[key]; seen {
		return answer
	}
	answer := xargsInputReachesUncached(tail, substituting, marker, names, memo)
	memo.xargsInput[key] = answer
	return answer
}

func xargsInputReachesUncached(tail []*syntax.Word, substituting bool, marker string, names map[string]struct{}, memo operandTailMemo) bool {
	if substituting && xargsWordCarriesMarker(tail[0], marker) {
		// The program this link runs is the substituted line.
		return true
	}
	var next []*syntax.Word
	switch {
	case isAccountCommandName(tail[0], "env"):
		if memo.envArgvSuffixUnprovable(tail[1:]) {
			// The walk refuses an env argv it cannot parse.
			return false
		}
		scan := memo.envScan(tail[1:], envcommand.Start, names)
		if scan.refused {
			return false
		}
		if scan.command == nil {
			// unwrapXargs's env scan refuses appended items here.
			return !substituting
		}
		next = scan.command
	case isAccountCommandName(tail[0], "strace"):
		command, refuse := memo.straceInputRegion(tail[1:], substituting, marker)
		if refuse {
			return true
		}
		if command < 0 {
			return !substituting
		}
		next = tail[1+command:]
	default:
		return false
	}
	peeled, unsafe := unwrapAccountCommand(next, names, memo)
	if unsafe {
		// Behind env the walk refuses this itself; behind strace it never
		// peels the child, so the refusal has to come from here whenever
		// input can reach the link.
		return !substituting || memo.xargsMarkerAnywhere(next, marker)
	}
	return xargsInputReaches(peeled, substituting, marker, names, memo)
}

// straceEnvNameCarries reports whether the marker sits in the VAR part of an
// -E/--env value (VAR or VAR=VAL), so the line picks the variable.
func straceEnvNameCarries(value, marker string) bool {
	name, _, _ := strings.Cut(value, "=")
	return strings.Contains(name, marker)
}

// straceShortValueFlags are the strace 6.8 short options that take an
// argument (optstring "+a:Ab:cCdDe:E:fFhiI:kno:O:p:P:qrs:S:tTu:U:vVwxX:yYzZ",
// read from the binary).
const straceShortValueFlags = "abeEIoOpPsSuUX"

// straceValue is what the word after a value-taking strace option is.
type straceValue uint8

const (
	straceNoValue straceValue = iota
	straceDataValue
	straceEnvValue
	// straceUnresolvedValue follows a --name the tables cannot resolve: the
	// next word may be its value or strace's command.
	straceUnresolvedValue
)

// straceInputRegion walks strace's option region — strace stops parsing
// options at its first non-option word, the traced command — and returns that
// command's index in words, or -1 when the region runs to the end of words.
// refuse is set when a marker can put a program in strace's command slot, or
// can reach -E/--env with a command after it to run: as the name of the
// variable that option sets or removes, or as an option word whose identity
// the substituted line decides. A marker only in the value of a fixed name is
// data; a fixed denied name is the unmodeled-wrapper scan's refusal already.
// The region belongs to one chain link and xargsInputReaches visits each link
// once per input mode, so the walk is linear overall.
func (memo operandTailMemo) straceInputRegion(words []*syntax.Word, substituting bool, marker string) (int, bool) {
	pending := straceNoValue
	for idx, word := range words {
		literal, ok := literalShellWord(word)
		if !ok {
			// A dynamic word here is judged by the walk's "$x" rule.
			return idx, false
		}
		carries := substituting && strings.Contains(literal, marker)
		childFollows := memo.commandCanFollow(words[idx+1:])
		if pending != straceNoValue {
			if carries && childFollows {
				switch {
				case pending == straceEnvValue && straceEnvNameCarries(literal, marker),
					pending == straceUnresolvedValue:
					return 0, true
				}
			}
			pending = straceNoValue
			continue
		}
		if carries {
			target, known := optionMarkerTarget(literal, marker, straceShortValueFlags)
			switch {
			case strings.HasPrefix(literal, marker) || (known && target == ""):
				// The traced program itself, or — when the marker opens the
				// word — possibly an option the line spells
				// (`-ECODEX_HOME=1`) in front of the command.
				return 0, true
			case !known:
				if childFollows {
					return 0, true
				}
			case target == "-E":
				_, value, _ := strings.Cut(literal, "E")
				if childFollows && straceEnvNameCarries(value, marker) {
					return 0, true
				}
			case strings.HasPrefix(target, "--"):
				resolved, _, resolvedKnown := resolveLongOption(target[2:], straceLongOptions)
				_, value, _ := strings.Cut(literal, "=")
				if childFollows && (!resolvedKnown || resolved == "env") &&
					straceEnvNameCarries(value, marker) {
					return 0, true
				}
			}
		}
		switch {
		case literal == "--":
			if idx+1 < len(words) {
				return idx + 1, false
			}
			return -1, false
		case strings.HasPrefix(literal, "--"):
			name, _, attached := strings.Cut(literal[2:], "=")
			if attached {
				continue
			}
			resolved, requiresArg, known := resolveLongOption(name, straceLongOptions)
			switch {
			case !known:
				pending = straceUnresolvedValue
			case requiresArg && resolved == "env":
				pending = straceEnvValue
			case requiresArg:
				pending = straceDataValue
			}
		case strings.HasPrefix(literal, "-") && literal != "-":
			flags := literal[1:]
			for i := 0; i < len(flags); i++ {
				if strings.IndexByte(straceShortValueFlags, flags[i]) < 0 {
					continue
				}
				if i+1 == len(flags) {
					pending = straceDataValue
					if flags[i] == 'E' {
						pending = straceEnvValue
					}
				}
				break
			}
		default:
			return idx, false
		}
	}
	return -1, false
}

// commandCanFollow reports whether any word of words could be the program a
// wrapper runs: one that is dynamic or does not spell an option. Values of
// later options also pass, which only over-approximates. Memoized per
// position: each answer is its own word's or the next position's.
func (memo operandTailMemo) commandCanFollow(words []*syntax.Word) bool {
	answer := false
	scanned := 0
	for ; scanned < len(words); scanned++ {
		if cached, seen := memo.xargsCommandFollows[words[scanned]]; seen {
			answer = cached
			break
		}
	}
	for i := scanned - 1; i >= 0; i-- {
		if !answer {
			literal, ok := literalShellWord(words[i])
			answer = !ok || !strings.HasPrefix(literal, "-") || literal == "-"
		}
		memo.xargsCommandFollows[words[i]] = answer
	}
	return answer
}

// xargsMarkerAnywhere reports whether any word of words is dynamic or
// contains the marker anywhere — in a name, an option, or a value — memoized
// per (position, marker).
func (memo operandTailMemo) xargsMarkerAnywhere(words []*syntax.Word, marker string) bool {
	answer := false
	scanned := 0
	for ; scanned < len(words); scanned++ {
		if cached, seen := memo.xargsMarkerAnywhereMap[xargsMarkerKey{word: words[scanned], marker: marker}]; seen {
			answer = cached
			break
		}
	}
	for i := scanned - 1; i >= 0; i-- {
		if !answer {
			literal, ok := literalShellWord(words[i])
			answer = !ok || strings.Contains(literal, marker)
		}
		memo.xargsMarkerAnywhereMap[xargsMarkerKey{word: words[i], marker: marker}] = answer
	}
	return answer
}

// xargsShortValueFlags are GNU xargs's short options that take an argument,
// required (adEILnPs) or optional and attached (eli).
const xargsShortValueFlags = "adEILnPseli"

// xargsMarkerInNestedXargsOptions reports whether an outer marker sits in the
// option region of an xargs inside the command, where the substituted line
// decides the nested marker (-I/-i/--replace) or the variable
// --process-slot-var sets. Values of the other options — counts, sizes,
// delimiters, the arg file — are data. A nested xargs that names no command
// runs echo, so the danger needs a command after its option region. The scan
// matches xargs anywhere in the argv, which only over-approximates the
// command positions.
func (memo operandTailMemo) xargsMarkerInNestedXargsOptions(words []*syntax.Word, marker string) bool {
	for start, word := range words {
		if !isAccountCommandName(word, "xargs") {
			continue
		}
		options := words[start+1:]
		region := memo.xargsNestedRegion(options, marker, xargsNestedNone)
		if region.carries && memo.commandCanFollow(options[len(options)-region.rest:]) {
			return true
		}
	}
	return false
}

// xargsNestedRegion walks the xargs option region at the head of words from
// the given pending state, memoized per (position, marker, state) so that
// `-a xargs -a xargs …`, where every xargs word starts a scan over the rest,
// stays linear.
func (memo operandTailMemo) xargsNestedRegion(words []*syntax.Word, marker string, pending xargsNestedPending) xargsNestedResult {
	type step struct {
		key   xargsNestedKey
		local bool
	}
	var chain []step
	result := xargsNestedResult{rest: 0}
	for idx := 0; ; idx++ {
		if idx == len(words) {
			result = xargsNestedResult{rest: 0}
			break
		}
		key := xargsNestedKey{word: words[idx], marker: marker, pending: pending}
		if cached, seen := memo.xargsNested[key]; seen {
			result = cached
			break
		}
		literal, ok := literalShellWord(words[idx])
		if !ok {
			result = xargsNestedResult{rest: len(words) - idx}
			break
		}
		carries := strings.Contains(literal, marker)
		if pending != xargsNestedNone {
			chain = append(chain, step{key: key, local: carries && pending == xargsNestedDangerousValue})
			pending = xargsNestedNone
			continue
		}
		if literal == "--" {
			chain = append(chain, step{key: key})
			result = xargsNestedResult{rest: len(words) - idx - 1}
			break
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			result = xargsNestedResult{rest: len(words) - idx}
			break
		}
		local := false
		if carries {
			target, known := optionMarkerTarget(literal, marker, xargsShortValueFlags)
			local = !known || xargsNestedOptionDangerous(target)
		}
		chain = append(chain, step{key: key, local: local})
		if strings.HasPrefix(literal, "--") {
			name, _, attached := strings.Cut(literal[2:], "=")
			if attached {
				continue
			}
			resolved, requiresArg, known := resolveLongOption(name, xargsLongOptions)
			switch {
			case !known:
				pending = xargsNestedDangerousValue
			case requiresArg:
				pending = xargsNestedValue
				if resolved == "process-slot-var" {
					pending = xargsNestedDangerousValue
				}
			}
			continue
		}
		flags := literal[1:]
		for i := 0; i < len(flags); i++ {
			if strings.IndexByte(xargsShortValueFlags, flags[i]) < 0 {
				continue
			}
			if i+1 == len(flags) && strings.IndexByte("adEILnPs", flags[i]) >= 0 {
				pending = xargsNestedValue
				if flags[i] == 'I' {
					pending = xargsNestedDangerousValue
				}
			}
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		result.carries = result.carries || chain[i].local
		memo.xargsNested[chain[i].key] = result
	}
	return result
}

// xargsNestedOptionDangerous reports whether a nested xargs option, as
// optionMarkerTarget names it, takes the outer line where it reaches env:
// the replace marker or --process-slot-var's variable. A long name is
// resolved through getopt_long's prefixes (`--proc=`), and one the table
// cannot resolve counts as dangerous.
func xargsNestedOptionDangerous(target string) bool {
	switch target {
	case "-I", "-i":
		return true
	}
	if !strings.HasPrefix(target, "--") {
		return false
	}
	resolved, _, known := resolveLongOption(target[2:], xargsLongOptions)
	return !known || resolved == "replace" || resolved == "process-slot-var"
}
