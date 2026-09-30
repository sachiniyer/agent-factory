package sessionenv

import (
	"mvdan.cc/sh/v3/syntax"
)

// xargsSubstitutedArgv returns the argv xargs execs under -I/-i/--replace,
// with every initial argument the marker reaches spelled as a synthetic
// dynamic word (#4977). The marker is replaced by an input line this walk
// cannot see, so that word is one argv word of unknown content — exactly what
// the walk assumes of a double-quoted "$x" — and every existing rule for that
// shape now applies to it. A dynamic word in a modeled wrapper's command slot
// is refused because it can name env or a shell; one inside an unmodeled
// wrapper has the words after it judged as env's argv; and a nested xargs
// whose own -I argument the outer marker reaches has an unknown marker.
//
// Before this the marker stayed literal text and read as an ordinary program
// name: `xargs -I{} strace {} CODEX_HOME=/x codex` fed the line `env` ran
// `strace env CODEX_HOME=/x codex`, hidden from the unmodeled-wrapper scan,
// and `xargs -I{} nohup {} …` ran the line as nohup's program.
//
// xargs's own COMMAND word is never substituted: GNU xargs 4.9 replaces the
// marker only in INITIAL-ARGS (`printf 'env\n' | xargs -I{} {} A` tries to
// execute a literal `{}`), and BSD xargs does the same. So words[0] stays a
// literal program name, exactly as it would without xargs.
//
// A word carries the marker by xargsMarkerInName: before the word's first
// '=', or anywhere when the marker itself contains '='. A dynamic marker
// (`-I "$M"`) can match any word, so no command is provable.
//
// Nothing is copied when no initial argument carries the marker, so nested
// replace-mode xargs whose markers do not occur keep sharing the walk's
// per-suffix memos (Codex on #4979: 2,000 layers of `xargs -i` took 11s when
// every layer copied its suffix). When a word is substituted, every word is
// copied: operandTailMemo keys a suffix by its first word's pointer, and a
// suffix that contains an item is not the suffix the original pointer named.
// xargsMarkerLimitExceeded bounds how many markers can do that per walk.
func (memo operandTailMemo) xargsSubstitutedArgv(words []*syntax.Word, markerKnown bool, marker string) ([]*syntax.Word, bool) {
	if len(words) == 0 {
		return nil, false
	}
	if !markerKnown {
		return nil, true
	}
	if !memo.xargsLiteralMarkerFollows(words[1:], marker) {
		return words, false
	}
	argv := make([]*syntax.Word, len(words))
	for i, word := range words {
		if i > 0 {
			if value, literal := literalShellWord(word); literal && xargsMarkerInName(value, marker) {
				argv[i] = &syntax.Word{Parts: []syntax.WordPart{&syntax.DblQuoted{Parts: []syntax.WordPart{
					&syntax.ParamExp{Short: true, Param: xargsItemParam},
				}}}}
				continue
			}
		}
		copied := *word
		argv[i] = &copied
	}
	return argv, false
}

// xargsLiteralMarkerFollows reports whether any literal word of words carries
// the marker by xargsMarkerInName, answering each (position, marker) once per
// walk.
func (memo operandTailMemo) xargsLiteralMarkerFollows(words []*syntax.Word, marker string) bool {
	answer := false
	scanned := 0
	for ; scanned < len(words); scanned++ {
		if cached, seen := memo.xargsLiteralMarkers[xargsMarkerKey{word: words[scanned], marker: marker}]; seen {
			answer = cached
			break
		}
	}
	for i := scanned - 1; i >= 0; i-- {
		if !answer {
			value, literal := literalShellWord(words[i])
			answer = literal && xargsMarkerInName(value, marker)
		}
		memo.xargsLiteralMarkers[xargsMarkerKey{word: words[i], marker: marker}] = answer
	}
	return answer
}

// xargsItemParam names the synthetic "$xargs_item" parameter. The parser never
// produces this pointer, so it identifies a substituted marker and nothing else.
var xargsItemParam = &syntax.Lit{Value: "xargs_item"}

// isXargsItemWord reports whether word is a marker xargsSubstitutedArgv
// replaced.
func isXargsItemWord(word *syntax.Word) bool {
	if len(word.Parts) != 1 {
		return false
	}
	quoted, ok := word.Parts[0].(*syntax.DblQuoted)
	if !ok || len(quoted.Parts) != 1 {
		return false
	}
	exp, ok := quoted.Parts[0].(*syntax.ParamExp)
	return ok && exp.Param == xargsItemParam
}

// xargsItemFollows reports whether any word of words is a substituted xargs
// marker, answering each position once per walk the way
// envArgvSuffixUnprovable does.
func (memo operandTailMemo) xargsItemFollows(words []*syntax.Word) bool {
	answer := false
	scanned := 0
	for ; scanned < len(words); scanned++ {
		if cached, seen := memo.xargsItems[words[scanned]]; seen {
			answer = cached
			break
		}
	}
	for i := scanned - 1; i >= 0; i-- {
		answer = answer || isXargsItemWord(words[i])
		memo.xargsItems[words[i]] = answer
	}
	return answer
}

func hasXargsItemWord(words []*syntax.Word) bool {
	for _, word := range words {
		if isXargsItemWord(word) {
			return true
		}
	}
	return false
}

// xargsItemEnvPlaceholder stands in for a substituted marker while env's argv
// is parsed. It is neither an option nor an assignment, so env's parse ends at
// it at the latest.
const xargsItemEnvPlaceholder = "af-xargs-item"

// xargsReplaceChild judges the argv of an xargs whose options set a replace
// marker, and returns what the walk continues with. state.marker is the
// walk's long-standing reading; state.replaceMarker and
// state.replaceCancelled are the real binaries' (unwrapXargs explains the
// difference), and every reading is judged.
func xargsReplaceChild(words []*syntax.Word, state xargsLoopState, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	if state.replaceCancelled {
		// GNU appends input instead, so env's operand region and the chain
		// are judged the way appended input reaches them too: `xargs -I{}
		// -n2 env` runs `env ITEM…` and `xargs -I{} -n2 nohup` hands nohup
		// its program. BSD still substitutes, so the replace-mode checks
		// below run as well (Codex on #4979).
		appended := xargsLoopState{markerKnown: true, marker: "{}"}
		if xargsEnvOperandsFed(words, appended, names, memo) {
			return nil, true
		}
		if _, unsafe := xargsChild(words, false, "", names, memo); unsafe {
			return nil, true
		}
	}
	if state.replaceMarker != state.marker {
		// A later bare -i/--replace switched GNU's marker back to {}.
		gnu := xargsLoopState{substituting: true, markerKnown: state.markerKnown, marker: state.replaceMarker}
		if xargsEnvOperandsFed(words, gnu, names, memo) {
			return nil, true
		}
	}
	if !state.markerKnown || memo.xargsMarkerLimitExceeded(state.replaceMarker) {
		return nil, true
	}
	// Two analyses of the same substitution, each refusal-only, so both must
	// pass. xargsChild follows the chain with the marker as literal text
	// (#4978); xargsSubstitutedArgv spells each substituted initial argument
	// as an unknown word for the rest of the walk (#4977).
	tail, unsafe := xargsChild(words, true, state.replaceMarker, names, memo)
	if unsafe {
		return nil, true
	}
	argv, unsafe := memo.xargsSubstitutedArgv(words, state.markerKnown, state.replaceMarker)
	if unsafe {
		return nil, true
	}
	if len(argv) == len(words) && (len(argv) == 0 || argv[0] == words[0]) {
		// Nothing was substituted: argv is words, which xargsChild peeled.
		return tail, false
	}
	return argv, false
}
