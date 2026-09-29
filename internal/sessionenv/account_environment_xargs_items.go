package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// xargsSubstitutedArgv returns the argv xargs execs under -I/-i/--replace,
// with every word the marker reaches spelled as a synthetic dynamic word
// (#4977). The marker is replaced by an input line this walk cannot see, so
// that word is one argv word of unknown content — exactly what the walk
// assumes of a double-quoted "$x" — and every existing rule for that shape now
// applies to it. A dynamic word in a command slot, xargs's own or a modeled
// wrapper's, is refused because it can name env or a shell; one inside an
// unmodeled wrapper has the words after it judged as env's argv; and a nested
// xargs whose own -I argument the outer marker reaches has an unknown marker.
//
// Before this the marker stayed literal text and read as an ordinary program
// name: `xargs -I{} {} CODEX_HOME=/x codex` fed the line `env` ran
// `env CODEX_HOME=/x codex`, and `xargs -I{} strace {} CODEX_HOME=/x codex`
// hid the same env from the unmodeled-wrapper scan.
//
// A word carries the marker when it occurs before the word's first '='. A
// marker only in an assignment's value leaves the name fixed and feeds data,
// the same split unwrapXargs's env check uses. A dynamic marker (`-I "$M"`)
// can match any word, so no command is provable.
func xargsSubstitutedArgv(words []*syntax.Word, markerKnown bool, marker string) ([]*syntax.Word, bool) {
	if len(words) == 0 {
		return nil, false
	}
	if !markerKnown {
		return nil, true
	}
	// Every word is copied, not only the substituted ones: operandTailMemo
	// keys a suffix by its first word's pointer, and a suffix that contains an
	// item is not the suffix the original pointer named.
	argv := make([]*syntax.Word, len(words))
	for i, word := range words {
		if value, literal := literalShellWord(word); literal {
			namePart, _, _ := strings.Cut(value, "=")
			if strings.Contains(namePart, marker) {
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
