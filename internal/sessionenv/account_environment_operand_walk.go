package sessionenv

import (
	"path/filepath"

	"mvdan.cc/sh/v3/syntax"
)

// shadowedChildTailMutates is shadowedOperandTailMutates for a wrapper's
// returned CHILD tail — the real util-linux binary's own command line, reached
// by ionice's `--` and `default` arms and by tasksetCommandAfterMask after the
// mask. A shadowed `./ionice` with `shift N; exec "$@"` can discard any prefix
// of that tail, so the property enforced is:
//
//	the tail is refused if ANY suffix of it, judged as a command line by
//	wrapperOperandTailMutates, mutates the account environment.
//
// It does NOT fail closed on the tail's length. The childless bound does not
// apply here: this tail is an ordinary command's argv, and a command may take
// any number of operands, so `ionice echo a1 … a65` is not a mutation (Codex on
// #4708 — the capped scan refused it for its length alone).
//
// Judging every suffix literally is quadratic: each judgement re-reads the rest
// of the tail. Two things make this walk linear in the command's length instead.
//
// Only some suffixes need their own judgement. words[0]'s suffix is judged
// unconditionally. That judgement peels recognized wrappers and reaches
// unwrapAccountCommand's default arm at the first head that is not one — a
// builtin included — where unrecognizedWrapperHidesAccountAssignment inspects
// every LATER position for an env word, a non-literal word, a shell start and
// an `--opt=DENIED` value. A later suffix therefore needs its own judgement only
// when its head begins a verdict that scan does not see from before it
// (accountChildTailSuffixStartsVerdict). A long benign argv has no such word and
// costs one judgement.
//
// Those judgements are still per word, and a tail can hold nothing else:
// `ionice echo unset unset …` judged each `unset` over the whole remainder, and
// nested wrappers (`ionice echo ionice echo …`) chained one child-tail walk per
// level (Codex on #4708). So each tail position is scanned once per validation
// (memo.childTails caches "does any suffix from here mutate"), and the suffix
// judgements every child-tail walk starts share shadowedChildJudgementLimit per
// validation. Past it the walk fails closed. The bound counts verdict words, not
// operands: a real launch command carries a handful of wrapper or builtin names
// in a child's argv, never dozens. Total work is O(limit × length).
func shadowedChildTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	if judgeShadowedChildSuffix(words, names, memo) {
		return true
	}
	return laterChildSuffixMutates(words[1:], names, memo)
}

// shadowedChildJudgementLimit bounds the suffix judgements the child-tail walks
// of one validation start; see shadowedChildTailMutates. The work is
// proportional to it times the command's length.
const shadowedChildJudgementLimit = 32

// judgeShadowedChildSuffix judges one child-tail suffix. A suffix another walk
// already judged is free; a new one draws on the shared budget and fails closed
// once it is spent.
func judgeShadowedChildSuffix(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if answer, seen := memo.answers[words[0]]; seen {
		return answer
	}
	*memo.childJudgements++
	if *memo.childJudgements > shadowedChildJudgementLimit {
		return true
	}
	return wrapperOperandTailMutates(words, names, memo)
}

// laterChildSuffixMutates reports whether any suffix of words that begins at a
// verdict word mutates. Every words slice in one validation is a suffix of the
// call's Args, so words[0] names the answer, and memo.childTails holds it for
// every position already scanned. A scan stops at the first cached position or
// the first mutating suffix; every position it passed has the same answer as
// the one it stopped at (none of them began a mutating suffix), so each is
// cached and no position is scanned twice.
func laterChildSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	answer := false
	scanned := 0
	for scanned < len(words) {
		if cached, seen := memo.childTails[words[scanned]]; seen {
			answer = cached
			break
		}
		if accountChildTailSuffixStartsVerdict(words[scanned]) &&
			judgeShadowedChildSuffix(words[scanned:], names, memo) {
			answer = true
			scanned++
			break
		}
		scanned++
	}
	for _, word := range words[:scanned] {
		memo.childTails[word] = answer
	}
	return answer
}

// accountChildTailSuffixStartsVerdict reports whether judging
// wrapperOperandTailMutates at a tail word's own suffix position can begin a
// verdict that judging an earlier suffix does not already cover. An earlier
// suffix's judgement reaches unwrapAccountCommand's default arm, whose
// unrecognizedWrapperHidesAccountAssignment scan inspects every later position
// for an env word, a non-literal word, a shell start, and an `--opt=DENIED`
// option value, so a buried form of any of those is already caught. Each name
// listed here begins a verdict that scan does not analyze from inside:
//
//   - A non-literal word can expand to env, a mutating builtin, or a wrapper
//     name after word splitting, so judge its suffix directly.
//   - A recognized wrapper's operands are analyzed only when this suffix is
//     judged at the wrapper's position; unrecognizedWrapperHidesAccountAssignment
//     scrutinizes only env words, shells, and option values, not a buried
//     wrapper's own operands.
//   - A direct account-mutating builtin's mutation starts only at its own
//     position; `env` is the exception because that scan has an
//     explicit env case, but it is listed for parity with the same-position
//     form a bare env child tail takes at suffix 0.
//   - `strace`'s `-E`/`--env` option is handled only inside the strace context
//     of unrecognizedWrapperHidesAccountAssignment, which is words[0]-sensitive,
//     so a buried strace only matches when judged from strace's own suffix.
//   - A shell is unproven only when judged as the command head: the opaque-head
//     scan flags a shell only while words follow it, so a bare trailing shell
//     (which a shadowed wrapper can shift to and exec) is missed from any
//     earlier prefix and must be judged from its own suffix, where
//     shellCommandIsUnproven sees it as the head and refuses it.
//
// The dispatch controls how each listed name is matched, so the candidate
// filter follows the same shape the dispatch actually triggers on: an
// executable wrapper or shell is recognized by basename (isAccountCommandName
// uses filepath.Base; knownShellName is fed one), so a path form
// (/usr/bin/nice, /bin/sh) is the same command a shadowed wrapper can shift to
// and stays a candidate. A builtin is recognized by isBareName, which rejects
// any path component, so a path or empty operand that merely shares a basename
// with a builtin (filepath.Base("/tmp/test") == "test", filepath.Base("") ==
// ".") is an external binary, not the builtin, and is inert here: matching it
// by basename spent the verdict budget on operands that cannot trigger the
// verdict and refused a benign long child argv (#4708 Codex review). Builtins
// are therefore matched by exact bare name. The word's literal is read once,
// because this runs for every word of every child tail.
func accountChildTailSuffixStartsVerdict(word *syntax.Word) bool {
	value, literal := literalShellWord(word)
	if !literal {
		return true
	}
	if _, verdict := accountChildTailSuffixVerdictNames[filepath.Base(value)]; verdict {
		return true
	}
	_, verdict := accountChildTailSuffixBuiltinVerdictNames[value]
	return verdict
}

// accountChildTailSuffixVerdictNames lists the executable wrappers and shells
// a per-suffix wrapperOperandTailMutates judgement must still inspect after an
// earlier suffix's judgement has already covered a buried env word, shell, and
// option value. Every entry is matched by basename: the form the dispatch
// recognizes for wrappers (isAccountCommandName) and shells (knownShellName).
var accountChildTailSuffixVerdictNames = map[string]struct{}{
	// Recognized wrappers peeled by unwrapAccountCommand that are dispatched
	// by isAccountCommandName, so a path form is the same wrapper.
	"nohup": {}, "nice": {}, "timeout": {}, "setsid": {}, "stdbuf": {}, "ionice": {}, "taskset": {}, "xargs": {},
	// `env` is matched by isAccountCommandName in unwrappedAccountCommandMutates
	// and inside the buried-env arm of unrecognizedWrapperHidesAccountAssignment,
	// so a buried env word is judged from any prefix; it is listed here only
	// for the same-position form a bare env child tail takes at suffix 0.
	"env": {},
	// `strace`'s option-value mutation lives only inside the strace context of
	// unrecognizedWrapperHidesAccountAssignment, so a buried strace can begin a
	// verdict only when judged from strace's own suffix.
	"strace": {},
	// Shells knownShellName recognizes. The opaque-head scan flags a buried
	// shell only while words follow it, so a bare trailing shell — which a
	// shadowed wrapper can shift to and exec — is judged at its own suffix,
	// where shellCommandIsUnproven sees it as the head and refuses it.
	"ash": {}, "bash": {}, "csh": {}, "dash": {}, "fish": {},
	"ksh": {}, "mksh": {}, "sh": {}, "tcsh": {}, "zsh": {},
}

// accountChildTailSuffixBuiltinVerdictNames lists the builtins a per-suffix
// judgement must still inspect: the wrappers peeled by isBareName
// (exec/command/builtin) and the direct account-mutating and declaration
// builtins named in unwrappedAccountCommandMutates. Every entry is matched by
// exact bare name — the form isBareName recognizes — so a path or empty
// operand that shares only a basename with one of these does not spend the
// verdict budget.
var accountChildTailSuffixBuiltinVerdictNames = map[string]struct{}{
	// Recognized wrappers peeled by unwrapAccountCommand that are dispatched
	// by isBareName (shell builtins, never a path).
	"exec": {}, "command": {}, "builtin": {},
	// Direct account-mutating builtins named in unwrappedAccountCommandMutates.
	"unset": {}, "set": {}, "hash": {},
	"read": {}, "getopts": {}, "printf": {}, "let": {}, "mapfile": {}, "readarray": {},
	"wait": {}, "test": {}, "[": {},
	"eval": {}, ".": {}, "source": {}, "trap": {}, "alias": {}, "fc": {}, "history": {}, "enable": {},
	// The declaration builtins isAccountDeclarationBuiltin matches.
	"export": {}, "readonly": {}, "declare": {}, "typeset": {}, "local": {},
}
