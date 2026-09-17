package sessionenv

import (
	"mvdan.cc/sh/v3/syntax"
)

// The account-environment walk is a bounded evaluator, not a parser: it judges
// suffixes, descends into nested wrappers, and re-walks the same program once
// per parser variant. This file holds the meter that makes all of that finite,
// separated from the rules it meters so the bound can be read — and changed —
// in one place.

// accountEnvironmentEvaluationBudget bounds the work ONE VALIDATION may do
// before the walk fails closed, counted in a single unit: argv words judged.
// Every charge is in that unit — a recursive descent into a nested env command
// or wrapper tail charges the argv it re-reads, and each strace suffix
// judgment charges the length of the suffix it walks — so an attacker-sized
// input refuses inside a fixed budget instead of overflowing the stack,
// fanning out exponentially, or stalling validation.
//
// The number is measured, not chosen. Spending it in full costs ~0.25-0.34s on
// the review box, across every shape that reaches it: a flat 800-word strace
// argv 0.19s, 700 path-shaped words 0.25s, 60 calls of a 300-word argv 0.25s,
// 20 nested strace words 0.34s, 600 nested env words 0.32s. Every walk
// ValidateAccountEnvironmentCommand runs draws on this one meter, so that is a
// bound on the validation, not on a call inside it.
//
// The old 32,768 counted SLOTS rather than words: each strace suffix judgment
// charged one while walking its whole tail, so the advertised bound held per
// short call and nowhere else — a 1000-word argv measured 0.54s and 32 of them
// 18.9s, all nominally inside budget (#4466 review). Fixing the unit and
// raising the number together keeps an ordinary long argv (a few hundred
// words) accepted while the pathological shapes refuse in a fifth of a second.
const accountEnvironmentEvaluationBudget = 262144

// evaluationBudget is the shared work meter for one command walk. Every
// recursive descent into the walker draws on the same counter, so the total
// cost of validating one command stays bounded no matter how many wrapper
// layers or suffix judgments the walk reaches.
type evaluationBudget struct {
	work int
	// operandTails bounds the shadowed-wrapper operand walk. Every words slice
	// inside one validation is a suffix of a call's Args — the parser allocates
	// each Word once — so the first element's pointer names a distinct remaining
	// suffix, and the answer to "does the operand-onward tail mutate" depends
	// only on that suffix and on names, which is fixed for the whole walk.
	// Without it the same suffix is walked once by the operand check and again
	// by the enclosing unwrap loop's continuation, so nested value-taking
	// wrappers recurred exponentially (Codex on #4465: ~3s at depth 20 of
	// `nice -n nice ...`, unbounded at 25). The map holds its keys, so a Word
	// from the other parser variant can never reuse an address it names.
	operandTails map[*syntax.Word]bool
}
