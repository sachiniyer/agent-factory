package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// wrapperOptionKey names one iteration of a modeled wrapper's option loop: the
// wrapper, the word the iteration stands on, and the loop's own state there
// (nil for the stateless loops, ioniceProofScope for ionice, xargsLoopState
// for xargs). The loop's result from that iteration — what unwrapX returns —
// depends only on those three, because the walk is deterministic and every
// words slice is a suffix of the call's Args.
type wrapperOptionKey struct {
	wrapper string
	word    *syntax.Word
	state   any
}

// wrapperOptionRun memoizes one pass of a wrapper's option loop at every
// iteration it visits. A value-taking option judges its operand as a command
// head (wrapperOperandTailMutates), so `nice -n nice -n …` starts a nested
// nice layer at each operand, and each layer's loop re-walked the rest of the
// same option run: quadratic in the run's length (#4968). With every
// iteration memoized, a nested layer that lands on an iteration the outer loop
// will visit in the same state answers it once for both.
//
// The states an iteration can carry are the loop's initial state and the ones
// earlier options in the same run set, so a position holds at most a handful
// of entries per wrapper.
type wrapperOptionRun struct {
	memo    operandTailMemo
	wrapper string
	chain   []wrapperOptionKey
}

func (memo operandTailMemo) optionRun(wrapper string) *wrapperOptionRun {
	return &wrapperOptionRun{memo: memo, wrapper: wrapper}
}

// visit is called at the top of each loop iteration. It reports the memoized
// result when this iteration was already answered; otherwise it records the
// iteration so done can answer it.
func (r *wrapperOptionRun) visit(words []*syntax.Word, state any) (unwrapResult, bool) {
	key := wrapperOptionKey{wrapper: r.wrapper, word: words[0], state: state}
	if cached, seen := r.memo.wrapperOptions[key]; seen {
		return cached, true
	}
	r.chain = append(r.chain, key)
	return unwrapResult{}, false
}

// done records the loop's result for every iteration this pass visited and
// returns it. Every return from a memoized loop goes through done.
func (r *wrapperOptionRun) done(words []*syntax.Word, unsafe bool) ([]*syntax.Word, bool) {
	for _, key := range r.chain {
		r.memo.wrapperOptions[key] = unwrapResult{words: words, unsafe: unsafe}
	}
	return words, unsafe
}

// xargsLoopState is unwrapXargs's option-loop state: whether the command
// substitutes input lines, and the marker it substitutes when that is known.
type xargsLoopState struct {
	substituting bool
	markerKnown  bool
	marker       string
}

type xargsEnvKey struct {
	word  *syntax.Word
	state xargsLoopState
}

type xargsMarkerKey struct {
	word   *syntax.Word
	marker string
}

// xargsEnvOperandsFed reports whether any env word in xargs's command words
// has an operand region that xargs's input can reach — the check unwrapXargs
// applies after its options. It is memoized per (position, state) in one
// forward pass, so `xargs env xargs env …`, where every nested xargs scans the
// rest of the command, stays linear (#4968).
func xargsEnvOperandsFed(words []*syntax.Word, state xargsLoopState, names map[string]struct{}, memo operandTailMemo) bool {
	var chain []xargsEnvKey
	answer := false
	for ; len(words) > 0; words = words[1:] {
		key := xargsEnvKey{word: words[0], state: state}
		if cached, seen := memo.xargsEnvScans[key]; seen {
			answer = cached
			break
		}
		chain = append(chain, key)
		if isAccountCommandName(words[0], "env") && xargsEnvOperandsFedAt(words[1:], state, names, memo) {
			answer = true
			break
		}
	}
	for _, key := range chain {
		memo.xargsEnvScans[key] = answer
	}
	return answer
}

// xargsEnvOperandsFedAt judges the env invocation whose argv is words. It is
// envcommand.Parse over envArgvWord's literals — a parse error, an unprovable
// word anywhere, or a cleared environment refuses — read through the walk's
// per-position env memos instead of re-parsing the suffix per env word.
func xargsEnvOperandsFedAt(words []*syntax.Word, state xargsLoopState, names map[string]struct{}, memo operandTailMemo) bool {
	if memo.envArgvSuffixUnprovable(words) {
		return true
	}
	scan := memo.envScan(words, envcommand.Start, names)
	if scan.refused {
		return true
	}
	// The command word counts as operand region: a substituted item landing
	// there is re-parsed by env, and an item spelling NAME=value becomes an
	// assignment rather than a program name.
	operandEnd := len(words)
	if scan.command != nil {
		operandEnd = len(words) - len(scan.command) + 1
	}
	if state.substituting {
		if !state.markerKnown {
			return true
		}
		remaining := memo.xargsMarkerWord(words, state.marker)
		return remaining > 0 && len(words)-remaining < operandEnd
	}
	// Without substitution, input items append after the initial arguments —
	// straight into env's operand region when env's own argv names no
	// command, so `xargs env` can run `env ITEM` with ITEM spelling NAME=value.
	return scan.command == nil
}

// xargsMarkerWord finds the first word of words that a substituted input line
// could turn into part of env's name-or-option text: a non-literal word, or a
// literal whose part before the first '=' contains the marker (a marker in an
// assignment's value feeds data env cannot reinterpret as a mutation). It
// returns the length of the suffix starting at that word, or 0 when there is
// none, memoized per (position, marker).
func (memo operandTailMemo) xargsMarkerWord(words []*syntax.Word, marker string) int {
	var chain []xargsMarkerKey
	answer := 0
	for i := range words {
		key := xargsMarkerKey{word: words[i], marker: marker}
		if cached, seen := memo.xargsMarkers[key]; seen {
			answer = cached
			break
		}
		chain = append(chain, key)
		if xargsWordCarriesMarker(words[i], marker) {
			answer = len(words) - i
			break
		}
	}
	for _, key := range chain {
		memo.xargsMarkers[key] = answer
	}
	return answer
}

func xargsWordCarriesMarker(word *syntax.Word, marker string) bool {
	lit, ok := literalShellWord(word)
	if !ok {
		return true
	}
	namePart, value, _ := strings.Cut(lit, "=")
	// --unset's value is a name env removes, not data (#4978): `env
	// --unset={}` fed CODEX_HOME drops the account root.
	return strings.Contains(namePart, marker) ||
		(namePart == "--unset" && strings.Contains(value, marker))
}
