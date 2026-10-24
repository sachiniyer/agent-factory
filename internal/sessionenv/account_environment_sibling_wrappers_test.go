package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// commandMutatesAccountEnvironment (in account_environment_command.go) inspects
// a shell command string to decide whether it could mutate an account's selected
// environment. It models argv-passthrough wrappers (nohup, nice, timeout, setsid,
// stdbuf, ionice, taskset, xargs) by "unwrapping" them and inspecting the child
// command they exec. A shadowed `./<wrapper>` with `shift N; exec "$@"` can
// discard any prefix of the child and exec any literal suffix of it, so the child
// tail is scanned with shadowedChildTailMutates: every suffix is judged, and any
// suffix that begins a verdict word (env, unset, xargs, a modelled wrapper, a
// shell, ...) is judged as a command head.
//
// Commit 6375e102 (#4708) added shadowedChildTailMutates and applied it to
// unwrapIonice's `--`/default arms and tasksetCommandAfterMask, but left five
// structurally identical sibling wrappers (nohup, nice, timeout, setsid, stdbuf)
// returning their child tail WITHOUT scanning it. A buried mutating suffix
// behind an inert-looking head (the same shape ionice/taskset refuse) was
// accepted under those five — a pure consistency gap, since the scan
// infrastructure and the threat model are identical for all seven. This test
// pins the #4708 parity fix that hooks the five siblings into the same scan.
func TestBuriedSuffixDifferential(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool // true = refused (mutation), false = accepted (safe)
	}{
		// Table A: a two-word `xargs --process-slot-var CODEX_HOME` suffix
		// buried behind an inert `echo` head under each wrapper. ionice/taskset
		// refused this pre-fix; the five siblings accepted it. All seven now
		// refuse for parity.
		{"ionice echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"taskset 0x1 echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"nice echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"nohup echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"timeout 5 echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"setsid echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"stdbuf -o0 echo a a a xargs --process-slot-var CODEX_HOME codex", true},

		// Table B: a buried `unset CODEX_HOME` suffix. ionice/taskset refused
		// this pre-fix; the five siblings accepted it. All seven now refuse.
		{"ionice echo unset CODEX_HOME", true},
		{"taskset 0x1 echo unset CODEX_HOME", true},
		{"nice echo unset CODEX_HOME", true},
		{"nohup echo unset CODEX_HOME", true},
		{"timeout 5 echo unset CODEX_HOME", true},
		{"setsid echo unset CODEX_HOME", true},
		{"stdbuf -o0 echo unset CODEX_HOME", true},

		// Boundary: the attached `--opt=DENIED` form IS caught by all five
		// (unrecognizedWrapperHidesAccountAssignment's --opt=DENIED value scan),
		// so the gap is exactly the separate-word form the rows above exercise.
		{"nice echo xargs --process-slot-var=CODEX_HOME codex", true},
		{"nohup echo xargs --process-slot-var=CODEX_HOME codex", true},
		{"timeout 5 echo xargs --process-slot-var=CODEX_HOME codex", true},
		{"setsid echo xargs --process-slot-var=CODEX_HOME codex", true},
		{"stdbuf -o0 echo xargs --process-slot-var=CODEX_HOME codex", true},
	}
	for _, tc := range cases {
		got := commandMutatesAccountEnvironment(tc.command, names)
		require.Equal(t, tc.want, got, "command %q", tc.command)
	}
}

// TestValidateAccountEnvironmentCommand_SiblingWrappersInspectBuriedXargs
// mirrors TestValidateAccountEnvironmentCommand_IoniceTasksetOptionValueBranchesInspectBuriedXargs
// (account_environment_followup_test.go) but for the five sibling wrappers
// #4708 left uninspected: nohup, nice, timeout, setsid, stdbuf. Each must now
// REFUSE a buried `xargs --process-slot-var <DENIED>` (a shadowed `./<wrapper>`
// with `shift N; exec "$@"` lands on the xargs boundary) and a buried
// `unset <DENIED>`, the same shapes ionice/taskset already refused.
func TestValidateAccountEnvironmentCommand_SiblingWrappersInspectBuriedXargs(t *testing.T) {
	for _, command := range []string{
		// Buried two-word `xargs --process-slot-var CODEX_HOME` behind an inert
		// echo, under each of the five siblings (the bug report's Table A).
		"nice echo a a a xargs --process-slot-var CODEX_HOME codex",
		"nohup echo a a a xargs --process-slot-var CODEX_HOME codex",
		"timeout 5 echo a a a xargs --process-slot-var CODEX_HOME codex",
		"setsid echo a a a xargs --process-slot-var CODEX_HOME codex",
		"stdbuf -o0 echo a a a xargs --process-slot-var CODEX_HOME codex",
		// The `--` arm of each wrapper that has one: a shadowed wrapper can
		// discard the `--` and any prefix and exec the buried suffix.
		"nice -- echo a a a xargs --process-slot-var CODEX_HOME codex",
		"timeout 5 -- echo a a a xargs --process-slot-var CODEX_HOME codex",
		"setsid -- echo a a a xargs --process-slot-var CODEX_HOME codex",
		"stdbuf -o0 -- echo a a a xargs --process-slot-var CODEX_HOME codex",
		// The same shape with `unset CODEX_HOME` as the buried suffix (the bug
		// report's Table B parity probe).
		"nice echo unset CODEX_HOME",
		"nohup echo unset CODEX_HOME",
		"timeout 5 echo unset CODEX_HOME",
		"setsid echo unset CODEX_HOME",
		"stdbuf -o0 echo unset CODEX_HOME",
		// The mutation at a non-zero offset behind the inert head: a shadowed
		// wrapper may shift more than one operand, so every literal suffix is
		// judged. Mirrors the ionice/taskset non-zero-offset rows.
		"nice echo true xargs --process-slot-var CODEX_HOME codex",
		"nohup echo true xargs --process-slot-var CODEX_HOME codex",
		"timeout 5 echo true xargs --process-slot-var CODEX_HOME codex",
		"setsid echo true xargs --process-slot-var CODEX_HOME codex",
		"stdbuf -o0 echo true xargs --process-slot-var CODEX_HOME codex",
		// A path-qualified shadowed wrapper is the same wrapper a shadowed
		// child can exec, so the scan must still fire.
		"/usr/bin/nice echo a a a xargs --process-slot-var CODEX_HOME codex",
		"./nohup echo unset CODEX_HOME",
		// A buried modelled wrapper whose own child mutates is refused too, for
		// parity with the ionice-in-ionice test (a shadowed wrapper can shift
		// to the inner wrapper boundary and exec its mutating child).
		"nice echo ionice -c 3 env CODEX_HOME=/other codex",
		"nohup echo taskset 0x1 env CODEX_HOME=/other codex",
		// Parity with the already-hardened siblings: the ionice/taskset rows
		// the existing test pins must still refuse after the sibling fix.
		"ionice -t echo xargs --process-slot-var CODEX_HOME codex",
		"taskset 0xff echo xargs --process-slot-var CODEX_HOME codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q buries an identity mutation behind an inert head inside a sibling wrapper's child tail", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The buried-suffix scan must not over-refuse: a clean child tail (no identity
// mutation) still runs under the real binary, and a shadowed wrapper execs an
// ordinary command with it. These benign rows must stay ACCEPTED post-fix,
// mirroring the ionice/taskset benign rows in
// account_environment_child_tail_budget_test.go and
// account_environment_followup_test.go.
func TestValidateAccountEnvironmentCommand_SiblingWrappersBuriedScanStaysNarrow(t *testing.T) {
	for _, command := range []string{
		// Common wrapper invocations with ordinary children.
		"nice -n 5 ./build",
		"nice make -j 8",
		"nice -n 10 npm run dev",
		"nohup ./server &",
		"nohup make build",
		"timeout 5 make -j",
		"timeout 10 ./test-suite",
		"setsid -f ./agent",
		"setsid -- make",
		"stdbuf -oL ./log-stream",
		"stdbuf -o0 ./daemon",
		// Simple echo children.
		"nice echo hello",
		"timeout 5 echo hello world",
		"setsid echo test",
		"stdbuf -o0 echo hi",
		"nohup echo hi",
		// Multi-word benign children: the scan judges every suffix and finds
		// no mutation.
		"nice -n 5 npm run build",
		"timeout 5 npm run dev",
		"setsid -- npm run dev",
		"stdbuf -oL npm run dev",
		// Option composition on the scanning wrappers.
		"nice -n 5 -n 10 npm run dev",
		"timeout 5 -k 10 npm run dev",
		"stdbuf -o0 -e0 ./daemon",
		"setsid -f -w ./agent",
		// A multi-word benign child that the scan walks linearly.
		"nice make -j 8 target1 target2 target3 target4 target5",
		// Long benign tail (past the childless bound but under the verdict
		// budget): the child-tail scan drops the childless PID bound, so
		// length alone is not a mutation.
		"nice echo a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a a codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q has no identity mutation and must stay allowed", command)
	}
}

// The child-tail suffix scan (#4708) shares one per-validation verdict budget
// (shadowedChildJudgementLimit = 32) across every wrapper that runs it. The five
// siblings now draw on the same budget ionice/taskset do, so a child tail whose
// verdict words exceed the budget fails closed. This pins the parity with the
// existing ionice/taskset budget behavior in
// account_environment_child_tail_budget_test.go.
func TestValidateAccountEnvironmentCommand_SiblingWrappersShareVerdictBudget(t *testing.T) {
	for _, prefix := range []string{
		"nice echo ",          // default arm: the tail starts at the first unset
		"nice -- echo ",       // `--` arm
		"timeout 5 echo ",     // default arm after the duration operand
		"timeout 5 -- echo ",  // `--` arm after the duration operand
		"setsid echo ",        // default arm
		"setsid -- echo ",     // `--` arm
		"stdbuf -o0 echo ",    // default arm after the -o operand
		"stdbuf -o0 -- echo ", // `--` arm
		"nohup echo ",         // the single child-return arm
	} {
		// At most limit-1 judgements (`echo`'s suffix plus one per unset): an
		// unset run inside the budget mutates nothing, so it stays admitted.
		admitted := prefix + strings.Repeat("unset ", shadowedChildJudgementLimit-2) + "x"
		require.NoError(t, ValidateAccountEnvironmentCommand(admitted, scopedProcessTabAccount()),
			"%q stays inside the verdict budget and mutates nothing", admitted)

		// Past the budget the scan fails closed: the wrapper's shadowable child
		// tail holds more verdict words than the budget, so this is refused.
		refused := prefix + strings.Repeat("unset ", shadowedChildJudgementLimit+1) + "x"
		err := ValidateAccountEnvironmentCommand(refused, scopedProcessTabAccount())
		require.Error(t, err, "%q spends past the verdict budget and must fail closed", refused)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
}
