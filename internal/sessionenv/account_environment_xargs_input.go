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
//     one link further in.
//   - strace's -E/--env puts a variable into the traced child's environment,
//     so input that can reach strace's option region — an appended item when
//     strace names no command yet, or a marker in or as an option word — can
//     override the account root: `xargs -I{} strace --env={} codex`.
//   - A nested xargs's -I and --process-slot-var are its own env-reaching
//     options, so an outer marker in its option region is refused.
//
// An unmodeled program's argv is outside this model: nothing here knows which
// unmodeled programs exec their arguments, and refusing input after every one
// of them would refuse `xargs rm`. Input there keeps the treatment the walk
// gives an unknown "$x" in the same place.

// xargsInputKey memoizes xargsInputReaches by chain suffix and input mode.
type xargsInputKey struct {
	word         *syntax.Word
	substituting bool
	marker       string
}

// xargsChild peels the modeled wrappers off the argv xargs execs and refuses
// it when xargs input can reach env through the chain. The peeled tail is
// returned so the caller does not peel it again.
func xargsChild(words []*syntax.Word, substituting bool, marker string, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	if substituting && xargsMarkerInNestedXargsOptions(words, marker) {
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
		command, refuse := straceInputRegion(tail[1:], substituting, marker)
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
		// A link the model cannot parse is one the real binary rejects, and
		// the walk already judges it where it runs a command (behind env).
		// strace's child was never peeled before this chain walk, so refusing
		// every unparseable one would refuse strace children xargs input
		// cannot reach; the verdict stays the walk's — unless appended items
		// are what completes the link: `xargs strace nice -n` hands the
		// first item to -n and the next to nice's command slot.
		return !substituting && xargsItemCompletesLink(next, names, memo)
	}
	return xargsInputReaches(peeled, substituting, marker, names, memo)
}

// xargsItemCompletesLink reports whether an unparseable chain parses once one
// appended item follows it, i.e. whether it failed only for want of a trailing
// operand. Every word is copied: operandTailMemo keys a suffix by its first
// word's pointer, and these suffixes end in a word the originals do not.
func xargsItemCompletesLink(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	extended := make([]*syntax.Word, 0, len(words)+1)
	for _, word := range words {
		copied := *word
		extended = append(extended, &copied)
	}
	extended = append(extended, &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: "1"}}})
	_, unsafe := unwrapAccountCommand(extended, names, memo)
	return !unsafe
}

// straceEnvNameCarries reports whether the marker sits in the VAR part of an
// -E/--env value (VAR or VAR=VAL), so the line picks the variable.
func straceEnvNameCarries(value, marker string) bool {
	name, _, _ := strings.Cut(value, "=")
	return strings.Contains(name, marker)
}

// straceShortValueFlags are the strace 6.8 short options that take an
// argument (optstring "+a:Ab:cCdDe:E:fFhiI:kno:O:p:P:qrs:S:tTu:U:vVwxX:yYzZ").
const straceShortValueFlags = "abeEIoOpPsSuUX"

// straceLongFlagsWithoutValue are the strace 6.8 long options that never take
// a separate argument word. Any other `--name` without '=' is assumed to take
// the next word, which only widens the option region: reading a value as a
// command would end the region early and miss a later -E.
var straceLongFlagsWithoutValue = map[string]bool{
	"follow-forks": true, "output-separately": true, "instruction-pointer": true,
	"stack-trace": true, "syscall-number": true, "output-append-mode": true,
	"relative-timestamps": true, "absolute-timestamps": true, "syscall-times": true,
	"no-abbrev": true, "strings-in-hex": true, "decode-fds": true, "decode-pids": true,
	"summary-only": true, "summary": true, "summary-wall-clock": true, "debug": true,
	"help": true, "seccomp-bpf": true, "tips": true, "version": true, "daemonize": true,
	"kill-on-exit": true, "successful-only": true, "failed-only": true,
}

// straceEnvLongName reports whether a long option name selects --env.
// getopt_long accepts any unambiguous prefix, so `--en=` is `--env=`.
func straceEnvLongName(name string) bool {
	return name != "" && strings.HasPrefix("env", name)
}

// straceInputRegion walks strace's option region — strace stops parsing
// options at its first non-option word, the traced command — and returns that
// command's index in words, or -1 when the region runs to the end of words.
// refuse is set when a marker can put a program in strace's command slot, or
// can reach -E/--env with a word after it to run: as the name of the
// variable that option sets or removes, or as an option word whose identity
// the substituted line decides. A marker only in the value of a fixed name is
// data; a fixed denied name is the unmodeled-wrapper scan's refusal already.
func straceInputRegion(words []*syntax.Word, substituting bool, marker string) (int, bool) {
	valuePending, envValuePending := false, false
	for idx, word := range words {
		literal, ok := literalShellWord(word)
		if !ok {
			// A dynamic word here is judged by the walk's "$x" rule.
			return idx, false
		}
		carries := substituting && strings.Contains(literal, marker)
		childFollows := commandCanFollow(words[idx+1:])
		if valuePending {
			if carries && envValuePending && childFollows && straceEnvNameCarries(literal, marker) {
				return 0, true
			}
			valuePending, envValuePending = false, false
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
			case strings.HasPrefix(target, "--") && straceEnvLongName(target[2:]):
				_, value, _ := strings.Cut(literal, "=")
				if childFollows && straceEnvNameCarries(value, marker) {
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
			if !attached && !straceLongFlagsWithoutValue[name] {
				valuePending, envValuePending = true, straceEnvLongName(name)
			}
		case strings.HasPrefix(literal, "-") && literal != "-":
			flags := literal[1:]
			for i := 0; i < len(flags); i++ {
				if strings.IndexByte(straceShortValueFlags, flags[i]) < 0 {
					continue
				}
				if i+1 == len(flags) {
					valuePending, envValuePending = true, flags[i] == 'E'
				}
				break
			}
		default:
			return idx, false
		}
	}
	return -1, false
}

// commandCanFollow reports whether any word could be the program a wrapper
// runs: one that is dynamic or does not spell an option. Values of later
// options also pass, which only over-approximates.
func commandCanFollow(words []*syntax.Word) bool {
	for _, word := range words {
		literal, ok := literalShellWord(word)
		if !ok || !strings.HasPrefix(literal, "-") || literal == "-" {
			return true
		}
	}
	return false
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
func xargsMarkerInNestedXargsOptions(words []*syntax.Word, marker string) bool {
	for start, word := range words {
		if !isAccountCommandName(word, "xargs") {
			continue
		}
		options := words[start+1:]
		carries, end := xargsNestedRegion(options, marker)
		if carries && commandCanFollow(options[end:]) {
			return true
		}
	}
	return false
}

// xargsNestedRegion walks the xargs option region at the head of words. It
// reports whether the marker reaches an env-reaching option there, and the
// index of the first word past the region (after a terminating "--").
func xargsNestedRegion(words []*syntax.Word, marker string) (bool, int) {
	carriesMarker, dangerousPending, valuePending := false, false, false
	for idx, option := range words {
		literal, ok := literalShellWord(option)
		if !ok {
			return carriesMarker, idx
		}
		carries := strings.Contains(literal, marker)
		if valuePending {
			if carries && dangerousPending {
				carriesMarker = true
			}
			valuePending, dangerousPending = false, false
			continue
		}
		if literal == "--" {
			return carriesMarker, idx + 1
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			return carriesMarker, idx
		}
		if carries {
			target, known := optionMarkerTarget(literal, marker, xargsShortValueFlags)
			if !known || xargsNestedOptionDangerous(target) {
				carriesMarker = true
			}
		}
		if strings.HasPrefix(literal, "--") {
			name, _, attached := strings.Cut(literal[2:], "=")
			switch name {
			case "arg-file", "delimiter", "max-args", "max-procs", "max-chars", "process-slot-var":
				if !attached {
					valuePending, dangerousPending = true, name == "process-slot-var"
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
				valuePending, dangerousPending = true, flags[i] == 'I'
			}
			break
		}
	}
	return carriesMarker, len(words)
}

func xargsNestedOptionDangerous(target string) bool {
	switch target {
	case "-I", "-i", "--replace", "--process-slot-var":
		return true
	}
	return false
}

// optionMarkerTarget names the option a marker-bearing word belongs to when
// the text before the marker fixes it: "--name" once a '=' precedes the
// marker, or "-X" for the first value-taking short flag before it. A word
// that does not start with '-' returns "" (an operand or the command). known
// is false when the substituted line can still choose the option: the marker
// opens the word, follows a bare "-" or "--name" with no '=', or sits among
// short flags before any value-taking one.
func optionMarkerTarget(literal, marker, shortValueFlags string) (string, bool) {
	prefix := literal[:strings.Index(literal, marker)]
	switch {
	case prefix == "" || prefix == "-":
		return "", false
	case strings.HasPrefix(prefix, "--"):
		name, _, attached := strings.Cut(prefix[2:], "=")
		if !attached {
			return "", false
		}
		return "--" + name, true
	case strings.HasPrefix(prefix, "-"):
		for _, flag := range prefix[1:] {
			if strings.ContainsRune(shortValueFlags, flag) {
				return "-" + string(flag), true
			}
		}
		return "", false
	default:
		return "", true
	}
}
