package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// straceOptionsRefused judges strace's OWN option words — the leading run up to
// the first word that is not an option or an option's operand — by inversion:
// an option word is admitted only when it is positively recognized as a known
// strace option, because strace can set or remove variables in its child's
// environment (-E/--env) and getopt_long accepts unambiguous abbreviations and
// short-option clusters, so enumerating the mutating spellings is a losing
// game (#5033). An unknown long option, an abbreviation, an unknown short
// flag, or a word whose option identity is unprovable refuses. Only -E/--env
// is judged further: its operand is refused when it names a denied variable or
// carries an assignment to one, or when its NAME is unprovable.
//
// A word that opens with an expansion is in option position until the command
// starts, and could expand to any option, so it refuses too. Options of other
// kinds keep an unprovable OPERAND only when it cannot field-split: a quoted
// operand (`-o"$LOG"`, `--output="$F"`) expands to a single argv word, so the
// operand of a known non-environment option cannot place a variable. An unquoted
// (field-splittable) operand (`--output=$F`, `-o$LOG`) can split into the
// operand plus further argv words — including `-E`/`--env` and a traced command
// — so it refuses the way a dash-glued unprovable identity does.
//
// The long-option table is the measured one the xargs input walk shares. Operands that arrive as the next word
// are consumed by arity; a wrong arity here can only skip a word this pass
// would have judged, never admit one the per-word scan refuses, because the
// caller still runs that scan over every tail word.
func straceOptionsRefused(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	return straceOptionRunRefused(words, names, true, memo)
}

// straceRunVerdict is the verdict of an option run entered at some word: clean
// when no earlier option was suspect, suspect when one was. An earlier suspect
// option refuses only if a traced command follows, so the two differ only
// where the run ends at a command word.
type straceRunVerdict struct{ clean, suspect bool }

type straceRunKey struct {
	word   *syntax.Word
	strict bool
}

// straceOptionRunRefused is straceOptionsRefused with the leading-expansion rule
// optional. Both callers pass refuseLeadingExpansion=true: a wholly-dynamic word
// in strace's option position could expand to a single -E/--env=NAME=VAL argv
// word, and the surrounding tail walk's unprovable-word path only judges the
// words AFTER it (never the hypothesis that the word itself expands to a strace
// env option), so the run must refuse such a word itself rather than end at it.
// A strace nested in another wrapper's tail sees the same tail walk that an
// unmodeled wrapper's child command position does, and that walk does not judge
// the dynamic word's own content as a strace option, so the nested run refuses a
// leading expansion the same way the outermost one does — symmetric with the
// head-level refusal of `strace "$OPT" codex`.
//
// The tail scan starts a run at every nested strace word, and a run that keeps
// finding operands (`-o strace -o strace …`) reaches the end of the command each
// time, so n nested stracas cost n² (Codex P2 on #5709). A run is a chain of
// positions whose verdict depends only on the words from there on, so each
// position's verdict is memoized (per leading-expansion mode) and a run stops at
// the first cached position.
func straceOptionRunRefused(words []*syntax.Word, names map[string]struct{}, refuseLeadingExpansion bool, memo operandTailMemo) bool {
	type link struct {
		word     *syntax.Word
		suspect  bool
		terminal *straceRunVerdict
	}
	var chain []link
	tail := straceRunVerdict{} // the run ran out of words: no command, nothing refused
	for i := 0; i < len(words); {
		if cached, seen := memo.straceRuns[straceRunKey{words[i], refuseLeadingExpansion}]; seen {
			tail = cached
			break
		}
		*memo.straceRunWork++
		suspect, next, terminal := straceOptionStep(words, i, names, refuseLeadingExpansion)
		chain = append(chain, link{words[i], suspect, terminal})
		if terminal != nil {
			break
		}
		i = next
	}
	verdict := tail
	for k := len(chain) - 1; k >= 0; k-- {
		switch {
		case chain[k].terminal != nil:
			verdict = *chain[k].terminal
		case chain[k].suspect:
			verdict = straceRunVerdict{clean: verdict.suspect, suspect: verdict.suspect}
		}
		memo.straceRuns[straceRunKey{chain[k].word, refuseLeadingExpansion}] = verdict
	}
	return verdict.clean
}

// straceOptionStep judges the single option word at words[i]. It reports
// whether that word is suspect, the index of the next word to judge, and — when
// the run ends here — the verdict of a run that ends at this word.
func straceOptionStep(words []*syntax.Word, i int, names map[string]struct{}, refuseLeadingExpansion bool) (suspect bool, next int, terminal *straceRunVerdict) {
	end := func(clean, suspectIn bool) (bool, int, *straceRunVerdict) {
		return false, 0, &straceRunVerdict{clean: clean, suspect: suspectIn}
	}
	if isXargsItemWord(words[i]) {
		// A substituted xargs item is judged by the xargs input walk
		// (straceInputRegion), which knows where the line's input lands.
		return end(false, false)
	}
	var prefix strings.Builder
	complete := true
	for _, part := range words[i].Parts {
		if !appendLiteralShellPart(&prefix, part) {
			complete = false
			break
		}
	}
	text := prefix.String()
	if text == "" {
		// A word that opens with an expansion is in option position and could
		// expand to any option (or be the command itself). In strict mode
		// (refuseLeadingExpansion=true, the only mode both callers now use)
		// it refuses: such a word can expand to a single -E/--env=NAME=VAL
		// argv word that injects or removes a protected variable, and the
		// tail walk cannot judge that hypothesis from the words after it.
		return end(refuseLeadingExpansion, true)
	}
	if !strings.HasPrefix(text, "-") {
		// Second reading (#7627): a word whose escape- and quote-normalized
		// spelling opens with a dash is an option the decode reveals — never
		// the traced command — so it is judged on the spelling the option
		// table reads, the way a name already is.
		if normalized := escapeQuoteNormalizedSpelling(text); normalized != text &&
			strings.HasPrefix(normalized, "-") {
			text = normalized
		} else {
			return end(false, true) // the traced command starts here
		}
	}
	if text == "--" && complete {
		return end(false, i+1 < len(words))
	}
	if strings.HasPrefix(text, "--") {
		name, operand, hasEq := strings.Cut(text[2:], "=")
		if !hasEq && !complete {
			// A literal long-option dash prefix glued to an unquoted parameter
			// expansion (`--$OPT`, `--en$OPT`) yields a non-empty name with
			// complete=false; at runtime the shell word-splits the unquoted
			// expansion inside the dash-glued word, so the option operand AND
			// a traced command can come from inside the variable even though
			// no literal argv word follows. A non-terminal suspect is dropped
			// when the run ends at this word, so this fails closed as a
			// terminal — symmetric with the wholly-dynamic `text == ""` case
			// and with the head-level refusal of `strace "$OPT" codex`.
			// A quoted expansion (`--"$OPT"`) is incomplete too but does NOT
			// field-split — it expands to a single argv word, so with no
			// command following strace runs nothing and cannot inject env; it
			// keeps the original non-terminal suspect so a command word after
			// it still refuses the way `strace --env=... codex` does.
			if wordHasUnquotedFieldSplittingExpansion(words[i]) {
				return end(refuseLeadingExpansion, true)
			}
			return true, i + 1, nil
		}
		// resolveLongOption reads the name the way getopt_long does — exact, or
		// an unambiguous prefix — so an abbreviation of --env resolves to env
		// instead of passing as an unknown name.
		resolved, requiresArg, known := resolveLongOption(name, straceLongOptions)
		if !known {
			// An unknown name with complete=false is the same risk shape
			// (`--unknown=$OPT`): the unquoted expansion can word-split into
			// the operand plus a traced command, and a non-terminal suspect is
			// dropped when the run ends at this word. A literal unknown name
			// (complete=true) keeps the existing non-terminal refusal so the
			// trace-nothing behavior for an unknown option with no command
			// following stays admitted. A quoted expansion
			// (`--unknown="$OPT"`) is incomplete but does not field-split, so
			// it keeps the original non-terminal suspect too.
			if wordHasUnquotedFieldSplittingExpansion(words[i]) {
				return end(refuseLeadingExpansion, true)
			}
			return true, i + 1, nil
		}
		if resolved == "env" {
			if hasEq {
				return straceEnvOperandRefused(operand, complete, names), i + 1, nil
			}
			value, width := stracePeekOperand(words, i)
			if width == 0 {
				return true, i + 1, nil
			}
			return straceEnvOperandRefused(value, true, names), i + 2, nil
		}
		// A known non-environment option whose word carries an unquoted
		// (field-splittable) expansion can split into the option operand PLUS
		// further argv words: `--output=$F` with
		// `F='trace -E CODEX_HOME=/other codex'` splits into `--output=trace`,
		// `-E`, `CODEX_HOME=/other`, `codex`, and strace processes the `-E`
		// before launching the literal command. The operand of a known
		// non-env option cannot by itself place a variable, but the split
		// argv can, so this fails closed as a terminal — the same shape as
		// the unprovable-identity paths above. A quoted operand
		// (`--output="$F"`) does not field-split and stays admitted.
		if wordHasUnquotedFieldSplittingExpansion(words[i]) {
			return end(refuseLeadingExpansion, true)
		}
		if !hasEq && requiresArg {
			return false, i + 2, nil // required operand arrives as the next word
		}
		return false, i + 1, nil
	}
	// A short-option cluster: flags until the first one that takes an operand,
	// which consumes the rest of the word.
	cluster := text[1:]
	for j := 0; j < len(cluster); j++ {
		c := cluster[j]
		if strings.IndexByte(straceShortFlags, c) >= 0 {
			continue
		}
		if strings.IndexByte(straceShortValueFlags, c) < 0 {
			break // a letter the table cannot place
		}
		operand := cluster[j+1:]
		attached := operand != "" || !complete
		switch {
		case c == 'E' && attached:
			return straceEnvOperandRefused(operand, complete, names), i + 1, nil
		case c == 'E':
			value, width := stracePeekOperand(words, i)
			if width == 0 {
				return true, i + 1, nil
			}
			return straceEnvOperandRefused(value, true, names), i + 2, nil
		case !attached:
			return false, i + 2, nil
		}
		// A known non-env value flag whose cluster carries an unquoted
		// (field-splittable) attached operand (`-o$LOG`) splits the same way
		// `--output=$F` does: `-o$LOG` with
		// `LOG='x -E CODEX_HOME=/other codex'` splits into `-ox`, `-E`,
		// `CODEX_HOME=/other`, `codex`. Fail closed as a terminal; a quoted
		// operand (`-o"$LOG"`) does not field-split and stays admitted.
		if wordHasUnquotedFieldSplittingExpansion(words[i]) {
			return end(refuseLeadingExpansion, true)
		}
		return false, i + 1, nil
	}
	// A cluster that never reached an operand-taking letter and still has an
	// unplaced or unprovable part has an unknown identity. With complete=false
	// the cluster carries an unquoted parameter expansion (`-$OPT`, `-f$OPT`,
	// `-Q$OPT`): the shell word-splits the unquoted expansion inside the
	// dash-glued word, so the option AND a traced command can come from inside
	// the variable even though no literal argv word follows. A non-terminal
	// suspect is dropped when the run ends at this word, so this fails closed as
	// a terminal — symmetric with the wholly-dynamic `text == ""` case and
	// with the long-option path above. A literal cluster (complete=true) keeps
	// the existing non-terminal refusal: an unknown short flag with no command
	// following runs nothing, so the trace cannot place an env mutation. A
	// quoted expansion (`-"$OPT"`) is incomplete but does not field-split, so
	// it keeps the original non-terminal suspect too.
	if wordHasUnquotedFieldSplittingExpansion(words[i]) {
		return end(refuseLeadingExpansion, true)
	}
	return strings.Trim(cluster, straceShortFlags) != "" || cluster == "", i + 1, nil
}

// straceEnvOperandRefused judges the var[=val] operand of -E/--env from its
// literal text. A literal NAME before an `=` decides whatever the value expands
// to; a NAME that may continue past the literal text (complete is false and no
// `=` has been seen — `-E$V`) is unprovable and refuses.
func straceEnvOperandRefused(operand string, complete bool, names map[string]struct{}) bool {
	if !complete && !strings.Contains(operand, "=") {
		return true
	}
	return accountEnvironmentOperandDenied(operand, names)
}

// stracePeekOperand returns the next word's literal text; width 0 means there
// is no next word or it is not provable.
func stracePeekOperand(words []*syntax.Word, i int) (string, int) {
	if i+1 >= len(words) {
		return "", 0
	}
	value, ok := literalShellWord(words[i+1])
	if !ok {
		return "", 0
	}
	return value, 1
}

// straceShortFlags take no operand; straceShortValueFlags (the xargs input
// walk's table) take the rest of the cluster, or the next word when nothing
// follows.
const straceShortFlags = "cCdDfFhiknqrtTvVwxyYzZA"

// wordHasUnquotedFieldSplittingExpansion reports whether the word contains an
// unquoted parameter, command, or arithmetic expansion whose runtime value
// undergoes shell field splitting — the only way a dash-glued word whose
// literal prefix is incomplete (complete=false) can split into more argv
// words than the source AST shows. Quoted expansions (`"$OPT"`, `"$(...)"`)
// and the non-splitting quote forms (`$'...'`, `$"..."`) do not field-split,
// so a word that is incomplete only because of those is not a splitting
// hazard and keeps its original (non-terminal) suspect path.
func wordHasUnquotedFieldSplittingExpansion(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch part.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp:
			return true
		}
	}
	return false
}
