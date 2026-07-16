package session

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #1917 follow-up locks: bounding the teardown's tmux commands means they can
// now answer "I don't know" instead of blocking forever — and an unknown must STOP
// the worktree step, not be logged and stepped over.
//
// Getting this wrong is worse than the bug the bound fixes: a kill that hangs is
// recoverable, a kill that deletes a live agent's worktree is not.

// gateStubMode records what the teardown core invoked. closeTab returns a
// caller-supplied error so each test can drive one classification.
type gateStubMode struct {
	closeErr         error
	worktreeCalled   bool
	finalizeCalled   bool
	clearStartedFlag bool
}

func (m *gateStubMode) closeTab(_ *tmux.TmuxSession, _, _ string) error { return m.closeErr }

func (m *gateStubMode) handleWorktree(_ *git.GitWorktree, _ string) error {
	m.worktreeCalled = true
	return nil
}

func (m *gateStubMode) clearsStarted() bool { return m.clearStartedFlag }

func (m *gateStubMode) finalize(_ *Instance, _ []closedTab, _ *git.GitWorktree) {
	m.finalizeCalled = true
}

func instanceWithTmuxTab(t *testing.T, ts *tmux.TmuxSession) *Instance {
	t.Helper()
	return &Instance{
		Title: "guarded",
		Tabs:  []*Tab{{ID: "tab-1", Name: agentTabName, Kind: TabKindAgent, tmux: ts}},
	}
}

// TestTeardownTabs_PaneMayBeLive_SkipsTheWorktreeStep is the gate itself: a tmux
// TIMEOUT means the pane's liveness is UNKNOWN, so the worktree action — delete
// for kill, move for archive — must not run at all.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the core called handleWorktree
// unconditionally after closeTab, so a wedged tmux server led straight to
// deleting the worktree of an agent that may still have been running in it.
func TestTeardownTabs_PaneMayBeLive_SkipsTheWorktreeStep(t *testing.T) {
	mode := &gateStubMode{
		closeErr: fmt.Errorf("tab %q: %w: kill-session after 10s", "agent", tmux.ErrTmuxTimeout),
	}
	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})

	err := inst.teardownTabs(mode)

	if mode.worktreeCalled {
		t.Fatal("the worktree action ran after tmux FAILED TO CONFIRM the pane was dead: " +
			"on a wedged tmux server this deletes (kill) or moves (archive) the workspace of " +
			"an agent that may still be running in it — data loss on a guess (#1917)")
	}
	if err == nil {
		t.Fatal("teardown reported success despite never confirming the pane was dead")
	}
	if !errors.Is(err, ErrPaneMayBeLive) {
		t.Fatalf("teardown error must be identifiable as a possibly-live pane so callers keep the record and retry, got: %v", err)
	}
	// finalize clears the tmux refs and the worktree pointer — exactly what a retry
	// needs to find intact.
	if mode.finalizeCalled {
		t.Fatal("finalize ran on the unsafe path; it clears the tmux refs a retry depends on")
	}
	if inst.Tabs[0].tmux == nil {
		t.Fatal("the tab's tmux ref was cleared, so a retry has nothing left to kill")
	}
}

// TestTeardownTabs_ConfirmedTmuxFailure_StillProceeds guards the other side, so
// the fix above cannot quietly become "any tmux hiccup blocks the kill". A tmux
// that ANSWERED with a failure means the session is gone or unkillable — teardown's
// goal either way — and #478's best-effort contract must still hold, or a stuck
// session becomes undeletable again.
func TestTeardownTabs_ConfirmedTmuxFailure_StillProceeds(t *testing.T) {
	mode := &gateStubMode{closeErr: nil} // teardownKill swallows answered failures
	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})

	if err := inst.teardownTabs(mode); err != nil {
		t.Fatalf("an answered tmux failure must stay best-effort (#478), got: %v", err)
	}
	if !mode.worktreeCalled {
		t.Fatal("the worktree action was skipped for a CONFIRMED-dead pane; the kill would never complete")
	}
	if !mode.finalizeCalled {
		t.Fatal("finalize must run on the normal path")
	}
}

// TestTeardownKill_ClassifiesTmuxTimeoutAsUnsafe drives the REAL teardownKill
// against a REAL TmuxSession whose tmux never answers, proving the production
// mode surfaces the timeout rather than swallowing it into a log line. Without
// this the gate above would sit behind a mode that never reports.
//
// It costs one real tmuxCommandTimeout (10s): the deadline is what produces
// ErrTmuxTimeout, and tmuxCommandTimeout is unexported to this package so it
// cannot be shortened from here. A mock that merely returns an error would prove
// nothing — the classification keys off ctx.Err(), so the deadline has to elapse.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: teardownKill.closeTab logged every
// CloseAndWaitForPaneExit error and returned nil, so the timeout never reached
// the core and the worktree step ran anyway.
func TestTeardownKill_ClassifiesTmuxTimeoutAsUnsafe(t *testing.T) {
	if testing.Short() {
		t.Skip("costs one real 10s tmux deadline")
	}
	wedged := cmd_test.MockCmdExec{
		// kill-session: never answers, so the bound trips and ctx.Err() is set.
		RunFunc: func(*exec.Cmd) error {
			time.Sleep(11 * time.Second)
			return fmt.Errorf("wedged tmux server never answered")
		},
		// panePID / list-panes: answer immediately so only ONE deadline elapses.
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("wedged tmux server never answered")
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("wedged-1917", "claude", nil, wedged)

	err := teardownKill{}.closeTab(ts, "guarded", "agent")
	if err == nil {
		t.Fatal("teardownKill swallowed a tmux TIMEOUT: the core then deletes the worktree of a " +
			"session tmux never confirmed dead (#1917). A timeout is not an ordinary best-effort failure.")
	}
	if !errors.Is(err, tmux.ErrTmuxTimeout) {
		t.Fatalf("the returned error must stay identifiable as a tmux timeout so the core can gate on it, got: %v", err)
	}
}
