package sessionenv

import (
	"path/filepath"

	"mvdan.cc/sh/v3/syntax"
)

// shadowedChildJudgementLimit bounds the suffix judgements the child-tail walks
// (shadowedLiteralChildTailMutates) of one validation start. Judging every
// suffix of a long tail would be quadratic, and nested wrappers chain one walk
// per level (Codex on #4708), so every walk's judgements share this budget per
// validation and the walk fails closed past it. It counts verdict words, not
// operands: a real launch command carries a handful of wrapper or builtin names
// in a child's argv, never dozens. Total work is proportional to it times the
// command's length.
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

// shadowedLiteralChildTailMutates judges a wrapper's real child argv by the
// exec-boundary rule: the boundary is decided at COMMAND POSITION, so words[0]
// is judged as a command (a dynamic word there refuses), and once it is literal
// its arguments are opaque — `ionice codex "$PROMPT"` runs codex with a prompt.
// A shadowed wrapper (`shift N; exec "$@"`) can still land on a LITERAL later
// word that begins a mutating command, so those suffixes are judged too.
func shadowedLiteralChildTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	if judgeShadowedChildSuffix(words, names, memo) {
		return true
	}
	return laterLiteralChildSuffixMutates(words[1:], names, memo)
}

// laterLiteralChildSuffixMutates judges each suffix of words that begins at a
// literal verdict word. A non-literal word is skipped: it is an argument, not
// the command the wrapper runs, so it is not an exec boundary. Every words slice
// in one validation is a suffix of the call's Args, so words[0] names the answer
// and memo.literalChildTails holds it for every position already scanned: the
// scan stops at the first cached position or the first mutating suffix, and each
// position it passed has that same answer, so no position is scanned twice
// (Codex P2 on #5711 — nested option interpretations returned one long tail
// once each).
func laterLiteralChildSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	answer := false
	scanned := 0
	for scanned < len(words) {
		if cached, seen := memo.literalChildTails[words[scanned]]; seen {
			answer = cached
			break
		}
		if _, literal := literalShellWord(words[scanned]); literal &&
			accountChildTailSuffixStartsVerdict(words[scanned]) &&
			judgeShadowedChildSuffix(words[scanned:], names, memo) {
			answer = true
			scanned++
			break
		}
		scanned++
	}
	for _, word := range words[:scanned] {
		memo.literalChildTails[word] = answer
	}
	return answer
}
