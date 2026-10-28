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
// kinds keep an unprovable OPERAND (`-o"$LOG"`, `--output=$F`): the operand of
// a known non-environment option cannot place a variable.
//
// The long-option table is the measured one the xargs input walk shares. Operands that arrive as the next word
// are consumed by arity; a wrong arity here can only skip a word this pass
// would have judged, never admit one the per-word scan refuses, because the
// caller still runs that scan over every tail word.
func straceOptionsRefused(words []*syntax.Word, names map[string]struct{}) bool {
	// suspect records an option word that is not provably safe. It refuses only
	// once a traced command follows: strace with options and no command runs
	// nothing, so there is no child whose environment could be placed.
	suspect := false
	for i := 0; i < len(words); i++ {
		if isXargsItemWord(words[i]) {
			// A substituted xargs item is judged by the xargs input walk
			// (straceInputRegion), which knows where the line's input lands.
			return false
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
			// A word that opens with an expansion is in option position and
			// could expand to any option (or be the command itself).
			return true
		}
		if !strings.HasPrefix(text, "-") {
			return suspect // the traced command starts here
		}
		if text == "--" && complete {
			return suspect && i+1 < len(words)
		}
		if strings.HasPrefix(text, "--") {
			name, operand, hasEq := strings.Cut(text[2:], "=")
			if !hasEq && !complete {
				suspect = true // the option name itself is unprovable
				continue
			}
			// resolveLongOption reads the name the way getopt_long does —
			// exact, or an unambiguous prefix — so an abbreviation of --env
			// resolves to env instead of passing as an unknown name.
			resolved, requiresArg, known := resolveLongOption(name, straceLongOptions)
			if !known {
				suspect = true
				continue
			}
			if resolved == "env" {
				if hasEq {
					suspect = suspect || straceEnvOperandRefused(operand, complete, names)
					continue
				}
				value, next := stracePeekOperand(words, i)
				if next == 0 {
					suspect = true
					continue
				}
				suspect = suspect || straceEnvOperandRefused(value, true, names)
				i++
				continue
			}
			if !hasEq && requiresArg {
				i++ // required operand arrives as the next word
			}
			continue
		}
		// A short-option cluster: flags until the first one that takes an
		// operand, which consumes the rest of the word.
		cluster := text[1:]
		placed := false
		for j := 0; j < len(cluster); j++ {
			c := cluster[j]
			if strings.IndexByte(straceShortFlags, c) >= 0 {
				continue
			}
			if strings.IndexByte(straceShortValueFlags, c) < 0 {
				break // a letter the table cannot place
			}
			placed = true
			operand := cluster[j+1:]
			attached := operand != "" || !complete
			switch {
			case c == 'E' && attached:
				suspect = suspect || straceEnvOperandRefused(operand, complete, names)
			case c == 'E':
				value, next := stracePeekOperand(words, i)
				if next == 0 {
					suspect = true
					break
				}
				suspect = suspect || straceEnvOperandRefused(value, true, names)
				i++
			case !attached:
				i++
			}
			break
		}
		// A cluster that never reached an operand-taking letter and still has
		// an unplaced or unprovable part has an unknown identity.
		if !placed {
			if !complete || strings.Trim(cluster, straceShortFlags) != "" || cluster == "" {
				suspect = true
			}
		}
	}
	return false
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
