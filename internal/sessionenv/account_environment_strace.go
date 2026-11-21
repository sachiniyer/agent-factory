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
// starts, and could expand to any option, so it refuses too. A word's verdict
// depends on how many argv words it can expand to: ANY word in option position
// or consumed as an option's operand that can expand to more than one argv
// word can supply more options and the traced command, so it refuses
// terminally — an unquoted field-splittable expansion (`--output=$F`,
// `-o$LOG`), a quoted `"$@"` or `"${a[@]}"`, and any quoted word containing
// one of those all qualify. Only a word provably expanding to exactly one
// argv word — a literal, a quoted `"$x"`/`"${x}"`/`"$*"`/`"${a[*]}"`, an
// arithmetic expansion, a process substitution — keeps its single-word
// judgement: a quoted operand (`-o"$LOG"`, `--output="$F"`) expands to one
// argv word, so the operand of a known non-environment option cannot place a
// variable.
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
	// A word that can expand to more than one argv word can supply more
	// options and the traced command no matter which option path it lands
	// on: an unquoted (field-splittable) expansion splits into the operand
	// PLUS further argv words — `--output=$F` with
	// `F='trace -E CODEX_HOME=/other codex'` splits into `--output=trace`,
	// `-E`, `CODEX_HOME=/other`, `codex`, and strace processes the `-E`
	// before launching the literal command; `-E$V`, `--env=$V` and
	// `--env=FOO=$X` split a -E/--env operand into the assignment plus the
	// same tail; and a quoted `"$@"`/`"${a[@]}"` yields one field per element
	// the same way. A non-terminal suspect is dropped when the run ends at
	// this word, so every such word — in the option name, an attached
	// operand, or a separate operand word — fails closed as a terminal,
	// symmetric with the wholly-dynamic `text == ""` case and with the
	// head-level refusal of `strace "$OPT" codex`. Only words provably
	// expanding to exactly one argv word reach the paths below: a quoted
	// expansion (`--"$OPT"`, `-o"$LOG"`, `--env="$V"`) keeps its original
	// non-terminal suspect so a command word after it still refuses the way
	// `strace --env=... codex` does.
	if wordYieldsMultipleArgvWords(words[i]) {
		return end(refuseLeadingExpansion, true)
	}
	if strings.HasPrefix(text, "--") {
		name, operand, hasEq := strings.Cut(text[2:], "=")
		if !hasEq && !complete {
			// The option name itself is unprovable: a quoted expansion glued
			// to the dashes (`--"$OPT"`) is incomplete but provably one word,
			// so it keeps the non-terminal suspect — a command word after it
			// still refuses; the multiword forms refused above.
			return true, i + 1, nil
		}
		// resolveLongOption reads the name the way getopt_long does — exact, or
		// an unambiguous prefix — so an abbreviation of --env resolves to env
		// instead of passing as an unknown name.
		resolved, requiresArg, known := resolveLongOption(name, straceLongOptions)
		if !known {
			// An unknown name keeps the non-terminal refusal so the
			// trace-nothing behavior for an unknown option with no command
			// following stays admitted; a quoted operand
			// (`--unknown="$OPT"`) is incomplete but provably one word.
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
		if !hasEq && requiresArg {
			// The required operand arrives as the next word. It must be
			// provably one argv word too: a multiword operand (`-o $F`,
			// `--output $F`, `-o "$@"`) supplies more options and the traced
			// command from inside the expansion, so it refuses terminally.
			if i+1 < len(words) && wordYieldsMultipleArgvWords(words[i+1]) {
				return end(refuseLeadingExpansion, true)
			}
			return false, i + 2, nil
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
			// Same separate-operand rule as the long-option path: the next
			// word must be provably one argv word.
			if i+1 < len(words) && wordYieldsMultipleArgvWords(words[i+1]) {
				return end(refuseLeadingExpansion, true)
			}
			return false, i + 2, nil
		}
		return false, i + 1, nil
	}
	// A cluster that never reached an operand-taking letter and still has an
	// unplaced or unprovable part has an unknown identity — the multiword
	// forms (`-$OPT`, `-f$OPT`, `-Q$OPT`, `-"$@"`) refused above. An
	// incomplete cluster has an unprovable identity too: its dynamic tail is
	// parsed as more option letters inside the same argv word, so `-f"$OPT"`
	// can expand to `-fECODEX_HOME=/o` — a single word carrying the env
	// operand — and keeps the non-terminal suspect so a command word after
	// it still refuses. A literal cluster (complete=true) keeps the
	// existing non-terminal refusal: an unknown short flag with no command
	// following runs nothing, so the trace cannot place an env mutation.
	return !complete || strings.Trim(cluster, straceShortFlags) != "" || cluster == "", i + 1, nil
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

// wordYieldsMultipleArgvWords reports whether the word can expand to more
// than one argv word at run time — the property that makes an option-position
// word refuse terminally, because the extra fields can supply more options
// and the traced command no matter how benign the literal text reads.
// Unquoted parameter and command expansions field-split on IFS; an extended
// glob matches several names; and inside double quotes a positional-@ or
// whole-array expansion (`"$@"`, `"${a[@]}"`, `"${!a[@]}"`, `"${!pre@}"`,
// `"${x:-$@}"`) still yields one field per element. A literal, a quoted
// ordinary expansion (`"$x"`, `"${x}"`, `"$*"`, `"${a[*]}"`, `"${#a[@]}"`),
// an arithmetic expansion (whose value is one number, even when computed
// from variables) and a process substitution (one /dev/fd path) each
// produce exactly one argv word.
func wordYieldsMultipleArgvWords(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ExtGlob:
			return true
		case *syntax.DblQuoted:
			if quotedPartsYieldMultipleFields(part.Parts) {
				return true
			}
		case *syntax.Lit, *syntax.SglQuoted, *syntax.ArithmExp, *syntax.ProcSubst:
			// Provably one argv word — see the comment above.
		default:
			return true
		}
	}
	return false
}

// quotedPartsYieldMultipleFields reports whether any part inside a
// double-quoted context can produce more than one field. Only the @-family
// of parameter expansions does: "$@", "${a[@]}" and the other multiword
// forms below. A quoted ordinary parameter expansion, command substitution
// or arithmetic expansion stays one word, and literal text is literal.
func quotedPartsYieldMultipleFields(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.ParamExp:
			if paramExpYieldsMultipleFieldsInsideQuotes(part) {
				return true
			}
		case *syntax.DblQuoted:
			if quotedPartsYieldMultipleFields(part.Parts) {
				return true
			}
		}
	}
	return false
}

// paramExpYieldsMultipleFieldsInsideQuotes reports whether a parameter
// expansion inside double quotes can produce more than one field: the
// positional-@ forms ("$@", "${@}", "${@:1}"), a whole-array [@] subscript
// ("${a[@]}", "${!a[@]}" — the @ name-list "${!pre@}"), and a nested
// multiword expansion inside a value-position operator word
// ("${x:-$@}" expands to one field per positional, measured on bash).
// "${a[*]}", "${!pre*}" and every length expansion ("${#@}", "${#a[@]}")
// join to a single word, and every pattern-position sub-word ("${y#$@}",
// "${y/$@/Q}") produces one field, so those stay single.
func paramExpYieldsMultipleFieldsInsideQuotes(exp *syntax.ParamExp) bool {
	if exp.Length {
		return false
	}
	if exp.Param != nil && exp.Param.Value == "@" {
		return true
	}
	if exp.Names == syntax.NamesPrefixWords {
		return true
	}
	if index, ok := exp.Index.(*syntax.Word); ok && index.Lit() == "@" {
		return true
	}
	return exp.Exp != nil && exp.Exp.Op <= syntax.AssignUnsetOrNull &&
		exp.Exp.Word != nil && quotedPartsYieldMultipleFields(exp.Exp.Word.Parts)
}
