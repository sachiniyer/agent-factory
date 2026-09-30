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

// xargsMarkerLimit bounds the distinct replace markers one walk follows.
// Every per-marker question (which words carry it, where the first one sits)
// is a scan of the remaining argv, memoized per (position, marker), so a
// command nesting n xargs with n different markers costs n scans: Codex on
// #4980 measured `xargs -IM000000Z xargs -IM000001Z … echo` at 18s for 2,000
// layers, and master's own env-operand scan has the same shape. Real commands
// nest one or two; past the limit the walk fails closed, as
// shadowedTailOperandLimit does for childless tails.
const xargsMarkerLimit = 8

// xargsMarkerLimitExceeded records marker as followed and reports whether the
// walk has now followed more distinct markers than xargsMarkerLimit.
func (memo operandTailMemo) xargsMarkerLimitExceeded(marker string) bool {
	memo.xargsMarkersFollowed[marker] = struct{}{}
	return len(memo.xargsMarkersFollowed) > xargsMarkerLimit
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
