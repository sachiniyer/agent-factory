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
// past shadowedTailOperandLimit fails closed instead.
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
