package sessionenv

import "regexp"

// plainCommandWord is a word that survives being re-joined with single spaces:
// no whitespace, quoting or shell metacharacters, so the result parses back to
// the same words.
var plainCommandWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,~-]+$`)

// StripDeclaredArgs returns program with the af-authored declared words removed:
// the clean base the launch started from. It answers yes only when it can PROVE
// the result is program minus exactly those words — program is a single literal
// simple command, the declared words appear in it as one contiguous block after
// the executable word, and what remains re-joins losslessly. Anything else
// (a wrapper it cannot place, a declaration that is not a contiguous block, a
// word that would need quoting) is a refusal, never a guess, so a caller that
// records the result as evidence never records a command that was not launched.
// An empty declaration returns program unchanged.
func StripDeclaredArgs(program string, declared []string) (string, bool) {
	call, ok := singleSimpleCall(program)
	if !ok || !callIsLiteral(call) || len(call.Assigns) > 0 {
		return "", false
	}
	words, ok := literalCommandArgs(call.Args)
	if !ok || len(words) == 0 {
		return "", false
	}
	if len(declared) == 0 {
		return program, true
	}
	start := -1
	for i := 1; i+len(declared) <= len(words); i++ {
		match := true
		for j, d := range declared {
			if words[i+j] != d {
				match = false
				break
			}
		}
		if match {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	kept := append(append([]string(nil), words[:start]...), words[start+len(declared):]...)
	out := ""
	for i, w := range kept {
		if !plainCommandWord.MatchString(w) {
			return "", false
		}
		if i > 0 {
			out += " "
		}
		out += w
	}
	return out, true
}
