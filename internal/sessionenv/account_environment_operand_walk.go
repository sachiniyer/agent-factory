package sessionenv

import "mvdan.cc/sh/v3/syntax"

// operandTailMemo bounds the shadowed-wrapper operand walk. Every words slice
// inside one validation is a suffix of the call's Args — the parser allocates
// each Word once — so the first element's pointer names a distinct remaining
// suffix, and the answer to "does the operand-onward tail mutate" depends only
// on that suffix. Without it the same suffix is walked once by the operand
// check and again by the enclosing unwrap loop's continuation, so nested
// value-taking wrappers recurred exponentially (Codex on #4465: ~3s at depth
// 20 of `nice -n nice ...`, unbounded at 25).
type operandTailMemo map[*syntax.Word]bool

// wrapperOperandTailMutates keeps a consumed option operand a candidate for
// inspection. The modeled wrappers match by basename, which cannot prove the
// binary is real util-linux: a repository-local or PATH-shadowed `ionice`
// containing `shift; exec "$@"` parses `-c` differently and executes what the
// model discarded as a class operand.
//
// words begins at the operand. A literal operand is judged as the head of a
// command line (`-c env -u CODEX_HOME codex` hides `env -u CODEX_HOME codex`
// under a shadowed binary). A dynamic operand is provably one argv word — the
// caller's gate — but the expansion itself is the shadowed command's HEAD:
// it can resolve to `env`, a same-shell builtin such as unset/export, or a
// shell that reads the tail as a script — exactly the position
// unwrappedAccountCommandMutates refuses outright. Judging only the env
// expansion left `ionice -c "$CLASS" /tmp/launch-agent` accepted while the
// literal `-c sh /tmp/launch-agent` refused (Codex on #4465), so the dynamic
// case fails closed. The option consumption that follows still covers the
// real binary's reading.
func wrapperOperandTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	if answer, seen := memo[words[0]]; seen {
		return answer
	}
	answer := wrapperOperandTailMutatesUncached(words, names, memo)
	memo[words[0]] = answer
	return answer
}

func wrapperOperandTailMutatesUncached(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if _, literal := literalShellWord(words[0]); !literal {
		return true
	}
	tail, unsafe := unwrapAccountCommand(words, names, memo)
	if unsafe {
		return true
	}
	if len(tail) == 0 {
		return false
	}
	return unwrappedAccountCommandMutates(tail, names, memo)
}

// shadowedTailOperandLimit bounds a childless tail. The real binaries take a
// handful of words there — PIDs and the odd permuted option; taskset takes one
// PID — and a saved command has no use for more: PIDs do not survive a
// restart, and `$(pgrep …)` is dynamic and already refused. The bound is per
// tail, so total work stays linear in the command's length.
const shadowedTailOperandLimit = 64

// shadowedOperandTailMutates fails closed when any word in a returned tail is
// not provably a single literal argv word — and when any literal boundary of
// that tail judges as a mutating command. It guards the childless tails —
// process-only selectors and terminal options — where the real util-linux
// binary consumes every remaining word as operand text (or never reaches
// them) and only the shadowed reading can execute one.
//
// Judging that tail from its first word alone let a literal operand mask what
// follows it: `./ionice -p"$PID" 123 "$CMD" /tmp/launch-agent` returned
// [123, "$CMD", ...] whose literal head read as an unrecognized command,
// while a repo-local ionice stripping a different operand count execs
// `sh /tmp/launch-agent` when CMD=sh (Codex on #4465). The all-literal case is
// the same hole with a named command: `./ionice -p 123 xargs
// --process-slot-var CODEX_HOME codex` is inert on the real binary, but a
// shadowed `shift 2; exec "$@"` lands on the xargs boundary and replaces the
// account root (Codex on #4465). A shadowed wrapper may discard ANY count of
// operands, so every literal suffix is a possible exec boundary and each is
// judged as one; the memoized wrapper walk keeps the scan polynomial.
//
// Each suffix judgment re-walks the rest of the tail, so the scan is quadratic
// in its length — 8000 literal PIDs took 8s against 6ms on master — and a tail
// past shadowedTailOperandLimit fails closed instead. That bound is a property
// of CHILDESS tails: PIDs and permuted operands are meaningless past a handful
// of words, so length there is a reasonable fail-closed signal.
func shadowedOperandTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) > shadowedTailOperandLimit {
		return true
	}
	for i := range words {
		if wrapperOperandTailMutates(words[i:], names, memo) {
			return true
		}
	}
	return false
}

// shadowedChildTailMutates is shadowedOperandTailMutates for a wrapper's
// returned CHILD tail — the real util-linux binary's own command line, reached
// by ionice's `--` and `default` arms and by tasksetCommandAfterMask after the
// mask. It judges every literal suffix as a possible exec boundary, exactly as
// shadowedOperandTailMutates does, but it does NOT fail closed on length.
//
// The childless bound does not apply here: this tail is an ordinary command's
// argv, and a command may legitimately take any number of operands, so a long
// child tail is not an environment mutation. Applying shadowedOperandTailMutates
// instead rejected `ionice echo a1 … a65` solely for having 65 arguments (and
// the `ionice --` branch for an even shorter list, since the child head lands in
// the scanned tail). A buried mutation anywhere in the tail is still refused —
// including past shadowedTailOperandLimit — but the scan no longer treats the
// count itself as a refusal and no longer walks every suffix literally.
//
// Judging EVERY suffix through wrapperOperandTailMutates is quadratic in the
// tail's length (Codex on #4708): each call re-scans the remainder through
// unrecognizedWrapperHidesAccountAssignment, so a long benign argv such as
// `ionice echo a1 … aN` (N in the thousands) stalls an apply/swap. The scan
// stays linear in the command's length by judging the full tail once — that
// pass's unrecognizedWrapperHidesAccountAssignment inspects every later
// position for an env word, a non-literal word, a shell start, and an
// `--opt=DENIED` option value, so a buried env/shell/option-value is already
// caught from any prefix — and then judging only the suffix starting at a word
// whose own name begins a verdict the full-tail pass does not already cover
// (see accountChildTailSuffixStartsVerdict). A long benign argv has none of
// those words, so nothing past the full-tail pass runs for it; a buried
// wrapper/mutator at any position still lands its own judgement.
func shadowedChildTailMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	if wrapperOperandTailMutates(words, names, memo) {
		return true
	}
	for i := 1; i < len(words); i++ {
		if !accountChildTailSuffixStartsVerdict(words[i]) {
			continue
		}
		if wrapperOperandTailMutates(words[i:], names, memo) {
			return true
		}
	}
	return false
}

// accountChildTailSuffixStartsVerdict reports whether judging
// wrapperOperandTailMutates at a tail word's own suffix position can begin a
// verdict the full-tail pass above does not already cover from a word before
// it. The full-tail pass's unrecognizedWrapperHidesAccountAssignment scan
// inspects every later position for an env word, a non-literal word, a shell
// start, and an `--opt=DENIED` option value, so a buried form of any of those
// is already caught. Each name listed here begins a verdict the full-tail scan
// does not analyze from inside:
//
//   - A non-literal word can expand to env, a mutating builtin, or a wrapper
//     name after word splitting, so judge its suffix directly.
//   - A recognized wrapper's operands are analyzed only when this suffix is
//     judged at the wrapper's position; unrecognizedWrapperHidesAccountAssignment
//     scrutinizes only env words, shells, and option values, not a buried
//     wrapper's own operands.
//   - A direct account-mutating builtin's mutation starts only at its own
//     position; `env` is the exception because the full-tail scan has an
//     explicit env case, but it is listed for parity with the same-position
//     form a bare env child tail takes at suffix 0.
//   - `strace`'s `-E`/`--env` option is handled only inside the strace context
//     of unrecognizedWrapperHidesAccountAssignment, which is words[0]-sensitive,
//     so a buried strace only matches when judged from strace's own suffix.
//
// Accepting either the basename form (isAccountCommandName) or the bare word
// form (isBareName) for every name is a deliberate over-approximation: a
// redundant candidate only adds one suffix judgement that returns false along
// the same path the dispatch would take, never a wrong verdict.
// isAccountDeclarationBuiltin covers the declaration builtins
// (export/readonly/declare/typeset/local) matched by bare word in
// unwrappedAccountCommandMutates.
func accountChildTailSuffixStartsVerdict(word *syntax.Word) bool {
	if _, literal := literalShellWord(word); !literal {
		return true
	}
	for _, name := range accountChildTailSuffixVerdictNames {
		if isAccountCommandName(word, name) || isBareName(word, name) {
			return true
		}
	}
	return isAccountDeclarationBuiltin(word)
}

// accountChildTailSuffixVerdictNames lists the literal command names a
// per-suffix wrapperOperandTailMutates judgement must still inspect after the
// full-tail pass has already covered a buried env word, shell, and option
// value. Wrappers (nohup/nice/timeout/setsid/stdbuf/ionice/taskset/xargs) match
// by basename in the dispatch; the remaining direct mutators match by bare
// word; accepting either form is harmless.
var accountChildTailSuffixVerdictNames = []string{
	// Recognized wrappers peeled by unwrapAccountCommand.
	"exec", "command", "builtin",
	"nohup", "nice", "timeout", "setsid", "stdbuf", "ionice", "taskset", "xargs",
	// Direct account-mutating builtins named in unwrappedAccountCommandMutates.
	// `env` is also matched inside the full-tail scan's buried-env arm, so a
	// buried env word is judged from any prefix; it is listed here only for the
	// same-position form a bare env child tail takes at suffix 0.
	"env",
	"unset", "set", "hash",
	"read", "getopts", "printf", "let", "mapfile", "readarray",
	"wait", "test", "[",
	"eval", ".", "source", "trap", "alias", "fc", "history", "enable",
	// `strace`'s option-value mutation lives only inside the strace context of
	// unrecognizedWrapperHidesAccountAssignment, so a buried strace can begin a
	// verdict only when judged from strace's own suffix.
	"strace",
}
