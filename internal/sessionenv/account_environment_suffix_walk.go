package sessionenv

import (
	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// operandTailMemo holds every per-suffix answer one account-command walk
// computes. Every words slice inside one validation is a suffix of the call's
// Args — the parser allocates each Word once and no walk builds a new slice —
// so the first element's pointer names a distinct remaining suffix, and each
// question below depends only on that suffix (plus the constant denied names).
//
// Each map answers one question at most once per position, which is what
// keeps the walk linear in the command's word count (#4966). Without them the
// same suffix was judged once per enclosing scan: nested value-taking
// wrappers recurred exponentially (Codex on #4465: ~3s at depth 20 of
// `nice -n nice ...`), and so did nested env words, through
// envCallMutatesAccountEnvironment → unwrapAccountCommand →
// unrecognizedWrapperHidesAccountAssignment → envCallMutatesAccountEnvironment
// (#4966: ~4s at 20 words of `echo env env … env x`, over a minute at 25).
type operandTailMemo struct {
	// answers: wrapperOperandTailMutates — does the command starting here mutate.
	answers map[*syntax.Word]bool
	// unwrapped: unwrapAccountCommand — where the peeled command starts.
	unwrapped map[*syntax.Word]unwrapResult
	// envCalls: envCallMutatesAccountEnvironment, per requireCommand.
	envCalls map[envCallKey]bool
	// envScans: env's own option/assignment scan from here, per scan state.
	envScans map[envScanKey]envScanSummary
	// envArgvUnprovable: does any word from here on fail envArgvWord.
	envArgvUnprovable map[*syntax.Word]bool
	// wrapperTails: the unrecognized-wrapper tail scan from here, per head kind.
	wrapperTails map[wrapperTailKey]bool
	// wrapperOptions: a modeled wrapper's option loop from here, per loop state.
	wrapperOptions map[wrapperOptionKey]unwrapResult
	// xargsEnvScans: unwrapXargs's env operand-region scan from here, per state.
	xargsEnvScans map[xargsEnvKey]bool
	// xargsMarkers: where the first marker-carrying word from here sits.
	xargsMarkers map[xargsMarkerKey]int
	// xargsInput: can xargs input reach env through the chain from here, per
	// input mode (#4978).
	xargsInput map[xargsInputKey]bool
	// xargsCommandFollows: could any word from here on be a program.
	xargsCommandFollows map[*syntax.Word]bool
	// xargsMarkerAnywhereMap: does any word from here on carry the marker,
	// anywhere in the word.
	xargsMarkerAnywhereMap map[xargsMarkerKey]bool
	// xargsNested: a nested xargs's option-region scan from here, per state.
	xargsNested map[xargsNestedKey]xargsNestedResult
}

func newOperandTailMemo() operandTailMemo {
	return operandTailMemo{
		answers:           map[*syntax.Word]bool{},
		unwrapped:         map[*syntax.Word]unwrapResult{},
		envCalls:          map[envCallKey]bool{},
		envScans:          map[envScanKey]envScanSummary{},
		envArgvUnprovable: map[*syntax.Word]bool{},
		wrapperTails:      map[wrapperTailKey]bool{},
		wrapperOptions:    map[wrapperOptionKey]unwrapResult{},
		xargsEnvScans:     map[xargsEnvKey]bool{},
		xargsMarkers:      map[xargsMarkerKey]int{},
		xargsInput:        map[xargsInputKey]bool{},

		xargsCommandFollows:    map[*syntax.Word]bool{},
		xargsMarkerAnywhereMap: map[xargsMarkerKey]bool{},
		xargsNested:            map[xargsNestedKey]xargsNestedResult{},
	}
}

type unwrapResult struct {
	words  []*syntax.Word
	unsafe bool
}

type envCallKey struct {
	word           *syntax.Word
	requireCommand bool
}

type envScanKey struct {
	word  *syntax.Word
	state envcommand.State
}

// envScanSummary is what envCallMutatesAccountEnvironment needs from
// envcommand.Parse over a suffix: refused when Parse errors or clears the
// environment (both refuse the call outright), denied when a mutation names a
// denied variable, and the suffix starting at env's command word, if any.
type envScanSummary struct {
	refused bool
	denied  bool
	command []*syntax.Word
}

type wrapperTailKey struct {
	word   *syntax.Word
	strace bool
}

// envArgvWord literalizes one env operand the way env itself parses it: a
// non-literal word that still spells a NAME= assignment keeps its name (the
// value is dynamic), and anything else is unprovable.
func envArgvWord(word *syntax.Word) (string, bool) {
	value, literal := literalShellWord(word)
	if literal {
		return value, true
	}
	name, assignment := shellWordAssignmentName(word)
	if !assignment {
		return "", false
	}
	return name + "=AF_DYNAMIC_VALUE", true
}

// envArgvSuffixUnprovable reports whether any word of words fails envArgvWord.
// It literalizes each word at most once per walk: the forward pass stops at
// the first position already answered, and every position it passed gets the
// answer of the chain it belongs to.
func (memo operandTailMemo) envArgvSuffixUnprovable(words []*syntax.Word) bool {
	answer := false
	scanned := 0
	for ; scanned < len(words); scanned++ {
		if cached, seen := memo.envArgvUnprovable[words[scanned]]; seen {
			answer = cached
			break
		}
	}
	for i := scanned - 1; i >= 0; i-- {
		if !answer {
			_, provable := envArgvWord(words[i])
			answer = !provable
		}
		memo.envArgvUnprovable[words[i]] = answer
	}
	return answer
}

// envScan runs envcommand's scan over words from state, one Advance per
// position, and summarizes it. The Step at a position depends only on that
// position's words and the state, so the summary is memoized per
// (position, state): env words nested inside another env's option run — `-u
// env -u env …`, or a chain of dynamic assignments each scanned by the wrapper
// tail — share one scan instead of re-parsing every suffix. The caller has
// already established that every word of words passes envArgvWord.
func (memo operandTailMemo) envScan(words []*syntax.Word, state envcommand.State, names map[string]struct{}) envScanSummary {
	type pending struct {
		key    envScanKey
		denied bool
	}
	var chain []pending
	var summary envScanSummary
	for len(words) > 0 {
		key := envScanKey{word: words[0], state: state}
		if cached, seen := memo.envScans[key]; seen {
			summary = cached
			break
		}
		arg, ok := envArgvWord(words[0])
		if !ok {
			summary = envScanSummary{refused: true}
			chain = append(chain, pending{key: key})
			break
		}
		rest := words[1:]
		operand := func() (string, bool) {
			if len(rest) == 0 {
				return "", false
			}
			value, ok := envArgvWord(rest[0])
			return value, ok
		}
		step, err := envcommand.Advance(arg, operand, state, envcommand.Policy{AllowAssignments: true})
		if err != nil || step.Clear {
			summary = envScanSummary{refused: true}
			chain = append(chain, pending{key: key})
			break
		}
		if step.Command {
			summary = envScanSummary{command: words}
			chain = append(chain, pending{key: key})
			break
		}
		denied := step.HasMutation && accountEnvironmentNameDenied(step.Mutation.Name, names)
		chain = append(chain, pending{key: key, denied: denied})
		words = words[step.Width:]
		state = step.Next
	}
	for i := len(chain) - 1; i >= 0; i-- {
		summary.denied = summary.denied || chain[i].denied
		memo.envScans[chain[i].key] = summary
	}
	return summary
}
