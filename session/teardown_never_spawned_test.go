package session

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #2985: a create that fails BEFORE tmux spawns anything used to be retained as a
// tombstone. The gate could not confirm a pane it had no way to ask about, an
// unknown correctly blocks the destructive step, and the failed create then held
// its title until the user cleared it — on exactly the machines where creates fail.
//
// The fix belongs at the gate rather than at the retention decision, and these
// tests pin both halves of why: the exemption fires for a session that provably
// never created a pane, and NOT for one that merely failed somewhere.

// neverSpawnedSession returns a TmuxSession whose Start failed while reading the
// tmux environment — after the name was proven absent and before new-session ran.
// That is the reported case, and it is built by driving the real Start rather than
// by setting a flag, so the test cannot pass on a fact production never records.
func neverSpawnedSession(t *testing.T, name string) *tmux.TmuxSession {
	t.Helper()
	// tmux's REAL absence answer: exit 1 carrying "can't find session". A bare exit
	// 1 is not it — policy and wrapper failures share that status while the session
	// is alive — and the exemption's strict re-probe rejects the ambiguous form.
	absent, err := exec.Command("sh", "-c",
		fmt.Sprintf("printf \"can't find session: %s\\n\" >&2; exit 1", name)).Output()
	_ = absent
	if err == nil {
		t.Fatal("fixture: expected the shell to exit 1")
	}
	answersAbsent := func(c *exec.Cmd) bool {
		for _, a := range c.Args {
			if a == "has-session" {
				return true
			}
		}
		return false
	}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if answersAbsent(c) {
				return err
			}
			return fmt.Errorf("tmux is unavailable")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if answersAbsent(c) {
				return nil, err
			}
			return nil, fmt.Errorf("permission denied talking to the tmux server")
		},
	}
	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, "claude", persistPtyFactory{t: t, cmdExec: execu}, execu)
	if err := ts.Start(t.TempDir()); err == nil {
		t.Fatal("fixture: this Start was supposed to fail before spawning")
	}
	if !ts.ProvenNoPane() {
		t.Fatal("fixture: the failed Start did not record that nothing was spawned")
	}
	return ts
}

// TestTeardownTabs_NeverSpawned_CompletesWithoutATombstone is acceptance criterion
// 1: the gate does not fire, so the rest of the skeleton runs — worktree action,
// then finalize — and the record can drop cleanly.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the gate asked tmux about a pane that never
// existed, could not get an answer on a machine with no working tmux, and returned
// ErrPaneMayBeLive — which manager_create's cleanup turns into a retained record.
func TestTeardownTabs_NeverSpawned_CompletesWithoutATombstone(t *testing.T) {
	mode := &gateStubMode{worktreeState: stateKnown}
	// The REAL close path, not the stub's: the exemption lives inside
	// closeTabForDestructiveTeardown, so a stubbed closeTab would step over the
	// thing under test.
	mode.stateByName = nil
	inst := instanceWithTmuxTab(t, neverSpawnedSession(t, "af_2985_teardown"))

	state, _, err := teardownKill{}.closeTab(inst.Tabs[0].tmux, "guarded", "agent")

	if state != stateKnown {
		t.Fatal("a session that provably never created a pane came back as an unknown, so the " +
			"destructive step is blocked and the failed create is retained as a tombstone holding " +
			"its title (#2985). There is no pane to confirm: nothing was ever spawned.")
	}
	if err != nil {
		t.Fatalf("a paneless session has no failure to report, got: %v", err)
	}
}

// TestTeardownTabs_NeverSpawned_RunsTheSkeletonToFinalize drives the REAL kill
// mode, because criterion 1 is about what happens AFTER the gate: the worktree
// must be dealt with and the refs cleared. Exempting at the retention end instead
// would have returned at the gate — before handleWorktree — leaving the freshly
// created worktree on disk with nothing pointing at it.
//
// The stub mode cannot show this: its closeTab replaces the very function the
// exemption lives in, so a stubbed run passes with or without the fix. Finalize
// clearing the tab's tmux ref is the observable that the skeleton reached the end.
func TestTeardownTabs_NeverSpawned_RunsTheSkeletonToFinalize(t *testing.T) {
	inst := instanceWithTmuxTab(t, neverSpawnedSession(t, "af_2985_skeleton"))

	// No worktree: teardownKill.handleWorktree treats that as nothing to remove and
	// reports KNOWN, which keeps this hermetic while still running the real path.
	if err := inst.teardownTabs(teardownKill{}); err != nil {
		t.Fatalf("a create that never spawned has nothing to refuse over, got: %v", err)
	}
	if inst.Tabs[0].tmux != nil {
		t.Fatal("finalize never ran, so the gate stopped the skeleton: the record is retained as a " +
			"tombstone holding its title, and on the create path the worktree it made is left with " +
			"nothing pointing at it (#2985)")
	}
}

// TestTeardownKill_NotProven_StillAsksTmux is acceptance criterion 2, and the one
// that catches a predicate that is too broad. This session never had Start called
// at all — the state every restored session, pending-cleanup handle and sibling tab
// is in — so nothing was proved about it and the gate must still establish
// liveness the hard way.
//
// It costs one real tmux deadline for the same reason the #1917 test beside it
// does: the unknown is derived from ctx.Err(), so the deadline has to elapse.
func TestTeardownKill_NotProven_StillAsksTmux(t *testing.T) {
	if testing.Short() {
		t.Skip("costs one real 10s tmux deadline")
	}
	ts := wedgedTmuxSession("af_2985_unproven")
	if ts.ProvenNoPane() {
		t.Fatal("a session nobody proved anything about must not claim it has no pane")
	}

	state, _, err := teardownKill{}.closeTab(ts, "guarded", "agent")

	if state != stateUnknown {
		t.Fatal("the exemption fired for a session whose pane liveness was never established. " +
			"That is the #2962 guarantee: an unreadable pane set refuses, or a live agent's " +
			"worktree is deleted under it (#2985 acceptance criterion 2)")
	}
	if !errors.Is(err, tmux.ErrTmuxTimeout) {
		t.Fatalf("the timeout must stay identifiable, got: %v", err)
	}
}
