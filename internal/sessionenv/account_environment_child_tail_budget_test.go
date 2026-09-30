package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/syntax"
)

// A child tail made of verdict words — `ionice echo unset unset …` — gave every
// word its own suffix judgement, and each judgement re-read the rest of the
// tail, so validation was quadratic in the operand count (Codex on #4708: 10k
// operands took 49s). Those judgements now share shadowedChildJudgementLimit
// per validation and fail closed past it. The boundary is exact, so it is
// asserted as a verdict rather than as a duration.
func TestValidateAccountEnvironmentCommand_ChildTailVerdictWordsShareOneBudget(t *testing.T) {
	unsets := func(n int) string { return strings.Repeat("unset ", n) + "x" }
	for _, prefix := range []string{
		"ionice echo ",       // default arm: the tail starts at the first unset
		"ionice -c3 echo ",   // default arm after a consumed class value
		"ionice -- echo ",    // `--` arm: the tail starts at echo
		"taskset 0x1 echo ",  // tasksetCommandAfterMask
		"taskset -c 0 echo ", // tasksetCommandAfterMask after -c
	} {
		// At most limit-1 judgements: echo's suffix (on the `--` and taskset
		// arms) plus one per unset.
		admitted := prefix + unsets(shadowedChildJudgementLimit-2)
		require.NoError(t, ValidateAccountEnvironmentCommand(admitted, scopedProcessTabAccount()),
			"%q stays inside the judgement budget and mutates nothing", admitted)

		refused := prefix + unsets(shadowedChildJudgementLimit+1)
		err := ValidateAccountEnvironmentCommand(refused, scopedProcessTabAccount())
		require.Error(t, err, "%q spends past the judgement budget and must fail closed", refused)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}

	// Nested wrappers draw on the same budget: each `ionice` in the child tail
	// starts a child-tail walk of its own, and without a shared budget each
	// level re-read the rest of the command.
	require.NoError(t, ValidateAccountEnvironmentCommand(
		"ionice echo ionice echo ionice echo x", scopedProcessTabAccount()))
	require.Error(t, ValidateAccountEnvironmentCommand(
		"ionice echo"+strings.Repeat(" ionice echo", shadowedChildJudgementLimit+1)+" x",
		scopedProcessTabAccount()))
}

// The work a child-tail walk does is bounded by the budget, not by the tail's
// length: the adversarial shapes stop after limit+1 judgements, and a benign
// argv of ordinary words costs exactly one, however long it is. The counters
// are the oracle, so this holds on any machine at any load.
func TestShadowedChildTailMutates_WorkIsBoundedByTheBudget(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	const n = 20000
	for _, tc := range []struct {
		name       string
		command    string
		unsafe     bool
		judgements int
	}{
		{"verdict words", "ionice echo" + strings.Repeat(" unset", n) + " x", true, shadowedChildJudgementLimit + 1},
		{"wrapper words", "ionice echo" + strings.Repeat(" nice", n) + " x", true, shadowedChildJudgementLimit + 1},
		{"nested wrappers", "ionice echo" + strings.Repeat(" ionice echo", n/2) + " x", true, shadowedChildJudgementLimit + 1},
		{"benign argv", "ionice echo" + strings.Repeat(" a", n) + " x", false, 1},
		{"benign argv, taskset", "taskset 0x1 echo" + strings.Repeat(" a", n) + " x", false, 1},
		// A mutation buried past a long benign argv is still judged: one
		// judgement for the tail, one for the xargs suffix.
		{"buried mutation", "ionice echo" + strings.Repeat(" a", n) + " xargs --process-slot-var CODEX_HOME codex", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := syntax.NewParser().Parse(strings.NewReader(tc.command), "")
			require.NoError(t, err)
			call := file.Stmts[0].Cmd.(*syntax.CallExpr)
			memo := newOperandTailMemo()
			_, unsafe := unwrapAccountCommand(call.Args, names, memo)
			require.Equal(t, tc.unsafe, unsafe)
			require.Equal(t, tc.judgements, *memo.childJudgements,
				"suffix judgements started by child-tail walks")
			require.LessOrEqual(t, len(memo.childTails), len(call.Args),
				"each tail position is scanned at most once")
		})
	}
}
