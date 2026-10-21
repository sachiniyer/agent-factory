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

// GeneratedArgsByInsertion is GeneratedArgsBetween's sibling for rewrites that
// are not positional extensions: the codex resume splice turns `codex -c k=v`
// into `codex resume --last -c k=v`, inserting words mid-command rather than
// appending them (#5238 review). Without it the rewrite would be "undescribable"
// — the declaration cleared, the account-scoped resume refused, the pane dead.
//
// The claim it certifies is narrower than a generic diff, on purpose. It answers
// yes only when EVERY word of base after the executable is already declared —
// so the inserted block lands among words af authored — and the rewrite is a
// pure insertion: a contiguous block of new words with every base word kept, in
// order. The merged declaration then still enumerates the whole final command.
//
// Same fail-closed contract as GeneratedArgsBetween: a rewrite that replaces,
// drops, or reorders a word, or a base carrying words af never declared, gets a
// refusal — never a guess about which words were af's.
func GeneratedArgsByInsertion(base, final string, declared []string) ([]string, bool) {
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
	if !ok || len(finalWords) <= len(baseWords) {
		return nil, false
	}
	// The executable is not negotiable, and every post-executable word of base
	// must be a declared one — an insertion among USER words is unprovable the
	// same way the extension path already treats them.
	if finalWords[0] != baseWords[0] {
		return nil, false
	}
	if len(baseWords)-1 != len(declared) {
		return nil, false
	}
	for i, want := range declared {
		if baseWords[i+1] != want {
			return nil, false
		}
	}
	// The insertion point is the first divergence after the executable; every
	// base word after it must survive verbatim as final's tail (insertion only —
	// a rewrite that also replaces or drops words is not describable this way).
	p := 1
	for p < len(baseWords) && baseWords[p] == finalWords[p] {
		p++
	}
	tail := len(baseWords) - p
	for i := 0; i < tail; i++ {
		if finalWords[len(finalWords)-tail+i] != baseWords[p+i] {
			return nil, false
		}
	}
	inserted := finalWords[p : len(finalWords)-tail]
	if len(inserted) == 0 {
		return nil, false
	}
	// Splice the inserted block into the declaration at the point it landed:
	// declared[:p-1] are the base words before the gap, declared[p-1:] after.
	out := make([]string, 0, len(declared)+len(inserted))
	out = append(out, declared[:p-1]...)
	out = append(out, inserted...)
	out = append(out, declared[p-1:]...)
	return out, true
}
