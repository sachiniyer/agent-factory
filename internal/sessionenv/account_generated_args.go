package sessionenv

// GeneratedArgsBetween reports the argument words a launcher appended to base to
// reach final, for Account.GeneratedArgs (#3083).
//
// It lives beside the guard that consumes the answer, and that is the point
// rather than convenience. Producer and consumer MUST tokenize identically: if
// the launcher split its own rewrite one way and the guard split the executed
// command another, the declaration would describe something other than what is
// checked, and the disagreement would surface as an unexplained 127 in a pane. So
// both go through singleSimpleCall and literalShellWord, and neither owns a
// second copy of the rules.
//
// This is a statement af makes about its OWN work — "I turned base into final,
// and here is precisely what I added" — computed where both strings are in hand.
// The launcher cannot be asked to remember a list; the two strings are what it
// actually has, and the difference between them is derivable without inspecting
// what the added words MEAN. Nothing here looks at flag names.
//
// It fails closed, and every refusal is a launch that stays refused rather than a
// launch that proceeds unverified:
//
//   - either string not a single simple command, or not fully literal
//   - final not a positional extension of base (a rewrite that changed an
//     existing word rather than appending, which is not what this can describe)
//   - base carrying assignments, or the two disagreeing on the executable
//
// A base equal to final yields no words and true: that is the ordinary
// non-claude launch, where af appends nothing.
func GeneratedArgsBetween(base, final string) ([]string, bool) {
	baseCall, ok := singleSimpleCall(base)
	if !ok || !callIsLiteral(baseCall) || len(baseCall.Assigns) > 0 {
		return nil, false
	}
	finalCall, ok := singleSimpleCall(final)
	if !ok || !callIsLiteral(finalCall) || len(finalCall.Assigns) > 0 {
		return nil, false
	}
	baseWords, ok := literalCommandArgs(baseCall.Args)
	if !ok || len(baseWords) == 0 {
		return nil, false
	}
	finalWords, ok := literalCommandArgs(finalCall.Args)
	if !ok || len(finalWords) < len(baseWords) {
		return nil, false
	}
	// EVERY leading word must be unchanged, not merely the executable. A rewrite
	// that edited one of the user's own arguments while appending is not "af added
	// these", and describing it that way would hand the guard a claim that does not
	// match the command.
	for idx, want := range baseWords {
		if finalWords[idx] != want {
			return nil, false
		}
	}
	added := finalWords[len(baseWords):]
	if len(added) == 0 {
		return nil, true
	}
	// A fresh slice: the caller stores this on an Account that outlives the parse,
	// and handing back a view into finalWords invites an aliasing surprise.
	out := make([]string, len(added))
	copy(out, added)
	return out, true
}
