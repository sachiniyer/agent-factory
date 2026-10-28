package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// setMutatesAccountEnvironment reports whether a `set` call switches the shell
// into keyword mode, where an assignment-shaped word written AFTER a command
// name is placed in that command's environment instead of staying an argument.
//
// This walk reads every later call under DEFAULT parsing rules, so the mode is
// not a mutation of its own — it silently invalidates every verdict that
// follows it. Under `set -k`, `codex CODEX_HOME=/other` is not the two-word
// call this walk sees: bash removes the assignment from codex's arguments and
// launches it with the replacement root. Refusing the switch is what keeps the
// rest of the walk meaningful; tracking the mode across calls instead would
// have to model the shell's own state machine.
//
// Deliberately narrow: a process tab runs an arbitrary user command, and an
// ordinary `set -e` prologue must keep working. Only keyword mode is refused.
//
// Option arity modelled by this scanner:
//
//	-o / +o   conditional arity — consumes the next word as a mode name ONLY
//	          when that word does not start with `-` or `+`. Real mode names
//	          (pipefail, noclobber, keyword, …) never start with either; when
//	          the next word does start with one it is another option that the
//	          scan must keep examining. This applies to both the standalone
//	          `-o` word and to `o` embedded in a minus-prefixed cluster.
//	          These are the only conditional-arity options; all others have
//	          fixed arity (zero).
//
// Keyword-mode tracking: bash processes options left to right; a later `-k`
// overrides an earlier `+k` and vice versa. The scanner tracks the running
// state rather than returning on the first `-k`, so a sequence like
// `set -k +k` is correctly seen as leaving keyword mode off.
func setOptionTaint(words []*syntax.Word) setOptionTaintReason {
	keywordMode := false
	for idx := 0; idx < len(words); idx++ {
		value, literal := literalShellWord(words[idx])
		if !literal {
			// An operand this parser cannot evaluate could expand to -k.
			return setOptionTaintReason{kind: setTaintUnprovable}
		}
		// `--` and the first non-option operand both end option parsing: every
		// word after one is a positional parameter, so `set -- -k` assigns the
		// string "-k" to $1 and enables nothing. A lone `-` is also a bash
		// option terminator ("assign any remaining arguments to the positional
		// parameters"); `set +e - -k` assigns "-k" to $1 and does NOT enable
		// keyword mode. A `+` prefix is a turn-OFF flag in bash, not a
		// non-option operand, so it does NOT end the scan: `set +e -k` still
		// enables keyword mode and must be caught by the loop below.
		if value == "--" || value == "-" || (!strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "+")) {
			if keywordMode {
				return setOptionTaintReason{kind: setTaintKeywordMode}
			}
			return setOptionTaintReason{}
		}
		// A long-form switch names its mode in the next word.
		//
		// `-o` has conditional arity: it consumes the following word as a mode
		// name ONLY when that word does not start with `-` or `+`. A real mode
		// name (pipefail, noclobber, keyword, …) never starts with either; a
		// word that does start with one is another option that the scan must
		// continue examining. When the next word is another option, `-o` behaves
		// as bare `-o` (prints current settings) and the shell processes the
		// following option normally — so `set +e -o -k` does enable keyword mode
		// via the `-k` that the `-o` branch must NOT swallow.
		if value == "-o" || value == "+o" {
			if idx+1 >= len(words) {
				// A bare `set -o` prints the current settings.
				continue
			}
			mode, ok := literalShellWord(words[idx+1])
			if !ok {
				return setOptionTaintReason{kind: setTaintUnprovable}
			}
			// Only treat the next word as the mode name when it cannot itself
			// be an option token. Mode names (pipefail, noclobber, …) never
			// start with `-` or `+`; a word that does start with one is an
			// option that must be examined on the next iteration.
			if strings.HasPrefix(mode, "-") || strings.HasPrefix(mode, "+") {
				continue
			}
			if mode == "keyword" {
				keywordMode = value == "-o"
			}
			if !knownBashSetOptionName(mode) {
				// bash aborts its option scan at an unrecognized -o/+o name
				// (it prints an error and returns non-zero): any earlier -k
				// stays ON and any later +k is never applied. The running
				// keywordMode state the scanner tracks is therefore not bash's
				// final state, so fail closed instead of consuming the invalid
				// name and continuing — `set -k -o nonsense +k` cannot use an
				// invalid name to flip keywordMode back off across a +k that
				// bash never applies. Carry the offending name so the refusal
				// can name it instead of the false generic message.
				return setOptionTaintReason{kind: setTaintUnrecognized, option: mode}
			}
			idx++
			continue
		}
		// Short options cluster, so a guard matching only a lone "-k" walks
		// straight past "-ek" (the #3402 lesson). Track the running state
		// rather than returning immediately, so a later `+k` can cancel an
		// earlier `-k` (bash processes options left to right and the last
		// setting wins: `set -k +k` leaves keyword mode off).
		//
		// When a minus-prefixed cluster contains `o`, it has the same
		// conditional arity as the standalone `-o`: if the following word does
		// not start with `-` or `+`, that word is the mode name (and is consumed
		// by advancing idx). A plus-prefixed cluster containing `o` (`+eo`)
		// behaves as `+o` and turns the named mode OFF.
		prefix := value[0]
		tail := value[1:]
		if strings.ContainsRune(tail, 'o') {
			if idx+1 >= len(words) {
				// No following word: bare cluster with `o`, prints settings.
			} else {
				mode, ok := literalShellWord(words[idx+1])
				if !ok {
					return setOptionTaintReason{kind: setTaintUnprovable}
				}
				if !strings.HasPrefix(mode, "-") && !strings.HasPrefix(mode, "+") {
					// The following word is a mode name; consume it.
					if mode == "keyword" {
						keywordMode = prefix == '-'
					}
					if !knownBashSetOptionName(mode) {
						// bash aborts its option scan at an unrecognized -o/+o
						// name embedded in a cluster exactly as it does for a
						// standalone -o: an earlier -k (in this cluster or a
						// prior word) persists and a later +k is never applied,
						// so the running keywordMode state is not bash's final
						// state. Fail closed rather than consume the invalid
						// name and keep scanning. Carry the offending name so
						// the refusal can name it instead of the false generic
						// message.
						return setOptionTaintReason{kind: setTaintUnrecognized, option: mode}
					}
					idx++
					// Fall through to the `k` check: the cluster may contain `k`
					// in addition to `o` (e.g. `-ko pipefail`), and bash applies
					// all cluster characters — those before `o` and those after `o`
					// when the consumed name is valid. Skipping the check here
					// would miss a `k` in the same cluster.
				}
				// The following word is another option (or we just consumed the
				// mode name); fall through to the `k` check below.
			}
		}
		if strings.ContainsRune(tail, 'k') {
			keywordMode = prefix == '-'
		}
	}
	if keywordMode {
		return setOptionTaintReason{kind: setTaintKeywordMode}
	}
	return setOptionTaintReason{}
}

// setOptionTaintKind classifies why a `set` scan could not prove the shell's
// final keyword-mode state.
type setOptionTaintKind int

const (
	setTaintNone setOptionTaintKind = iota
	setTaintUnprovable
	setTaintKeywordMode
	setTaintUnrecognized
)

type setOptionTaintReason struct {
	kind   setOptionTaintKind
	option string
}

// setMutatesAccountEnvironment is the bool view of setOptionTaint for the walk's
// own refusal decision; the reason (and, for an unrecognized long option name,
// the offending name) is read separately to render a refusal that names it.
func setMutatesAccountEnvironment(words []*syntax.Word) bool {
	return setOptionTaint(words).kind != setTaintNone
}

// knownBashSetOptionName reports whether name is one of the long-form option
// names accepted by `set -o`/`set +o` in bash. The list is the exact long-name
// column of `set -o` in GNU bash (identical in non-POSIX and `bash --posix`
// modes), which is the shell this keyword-mode guard models: keyword mode is a
// bash extension, so the runtime the guard protects on bash-as-`/bin/sh`
// deployments (macOS, some RHEL/CentOS) is bash. dash has no `keyword` entry
// and rejects `set -k` outright, so this recognizer is unreachable there — the
// keyword-mode arm of the guard is moot on dash.
//
// bash aborts its option scan at an unrecognized -o/+o name (it prints an
// error and returns non-zero), freezing any earlier -k ON and skipping any
// later +k. setMutatesAccountEnvironment models that abort by failing closed
// on an unrecognized name rather than consuming it and continuing, so a
// payload like `set -k -o nonsense +k` cannot use an invalid name to flip the
// scanner's running keywordMode state back off across a `+k` that bash never
// applies.
//
// An invented `no`-prefixed "negation" (nopipefail, noerrexit, …) is excluded:
// bash itself rejects those with `set: <name>: invalid option name`, so
// admitting them would re-introduce this exact class of false-safe. Long
// names use dashes, not underscores (`interactive-comments`, not
// `interactive_comments`).
func knownBashSetOptionName(name string) bool {
	_, ok := bashSetOptionNames[name]
	return ok
}

var bashSetOptionNames = map[string]struct{}{
	"allexport":            {},
	"braceexpand":          {},
	"emacs":                {},
	"errexit":              {},
	"errtrace":             {},
	"functrace":            {},
	"hashall":              {},
	"histexpand":           {},
	"history":              {},
	"ignoreeof":            {},
	"interactive-comments": {},
	"keyword":              {},
	"monitor":              {},
	"noclobber":            {},
	"noexec":               {},
	"noglob":               {},
	"nolog":                {},
	"notify":               {},
	"nounset":              {},
	"onecmd":               {},
	"physical":             {},
	"pipefail":             {},
	"posix":                {},
	"privileged":           {},
	"verbose":              {},
	"vi":                   {},
	"xtrace":               {},
}
