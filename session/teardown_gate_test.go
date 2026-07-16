package session

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
	closeState       teardownState
	closeErr         error
	worktreeState    teardownState
	worktreeCalled   bool
	finalizeCalled   bool
	clearStartedFlag bool
}

func (m *gateStubMode) closeTab(_ *tmux.TmuxSession, _, _ string) (teardownState, error) {
	return m.closeState, m.closeErr
}

func (m *gateStubMode) handleWorktree(_ *git.GitWorktree, _ string) (teardownState, error) {
	m.worktreeCalled = true
	return m.worktreeState, nil
}

func (m *gateStubMode) clearsStarted() bool { return m.clearStartedFlag }

func (m *gateStubMode) finalize(_ *Instance, _ []closedTab, _ *git.GitWorktree) {
	m.finalizeCalled = true
}

// wedgedTmuxSession returns a TmuxSession whose server never answers kill-session,
// so the bound trips and the state comes back unknown. panePID/list-panes answer
// immediately, so exactly ONE tmuxCommandTimeout elapses per teardown.
//
// A mock that merely returned an error would prove nothing: the unknown state is
// derived from ctx.Err(), so the deadline has to actually elapse. tmuxCommandTimeout
// is unexported to this package and cannot be shortened from here, which is what
// these tests pay 10s for.
func wedgedTmuxSession(name string) *tmux.TmuxSession {
	wedged := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error {
			time.Sleep(11 * time.Second)
			return fmt.Errorf("wedged tmux server never answered")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("wedged tmux server never answered")
		},
	}
	return tmux.NewTmuxSessionWithDeps(name, "claude", nil, wedged)
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
		closeState: stateUnknown,
		closeErr:   fmt.Errorf("tab %q: %w: kill-session after 10s", "agent", tmux.ErrTmuxTimeout),
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
	mode := &gateStubMode{closeState: stateKnown, closeErr: nil} // an ANSWERED tmux failure
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
	ts := wedgedTmuxSession("wedged-kill-1917")

	state, err := teardownKill{}.closeTab(ts, "guarded", "agent")
	if state != stateUnknown {
		t.Fatal("teardownKill reported a KNOWN state for a tmux TIMEOUT: the core then deletes the " +
			"worktree of a session tmux never confirmed dead (#1917). A timeout is not an ordinary " +
			"best-effort failure.")
	}
	if err == nil || !errors.Is(err, tmux.ErrTmuxTimeout) {
		t.Fatalf("the error must stay identifiable as a tmux timeout for the caller's message, got: %v", err)
	}
}

// TestTeardownTabs_ArchivePaneMayBeLive_SkipsTheMove is finding (1) of the second
// review: the archive path never got the kill path's gate.
//
// It matters MORE than the kill path, not less: archive is the default reap
// action, so it runs constantly. handleWorktree is where MoveWorktree lives, and
// it also rejects a nil worktree — so proving it never ran is exactly proving the
// move never happened.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: teardownArchive.closeTab logged every
// CloseAndWaitForPaneExit error and returned nil, so the core moved the worktree
// of a session tmux had never confirmed dead.
func TestTeardownTabs_ArchivePaneMayBeLive_SkipsTheMove(t *testing.T) {
	if testing.Short() {
		t.Skip("costs one real 10s tmux deadline")
	}
	inst := instanceWithTmuxTab(t, wedgedTmuxSession("wedged-archive-1917"))
	inst.gitWorktree = nil // handleWorktree would REJECT this; the gate must run first

	err := inst.teardownTabs(teardownArchive{dest: t.TempDir()})

	if err == nil {
		t.Fatal("archive reported success though tmux never confirmed the pane was dead")
	}
	if !errors.Is(err, ErrPaneMayBeLive) {
		t.Fatalf("archive must report a possibly-live pane so the daemon rolls back and retries, got: %v", err)
	}
	// If handleWorktree had run it would have returned "cannot archive … no worktree
	// to relocate". Its absence proves MoveWorktree was never reached.
	if strings.Contains(err.Error(), "cannot archive") {
		t.Fatal("handleWorktree ran despite tmux never confirming the pane was dead: on a wedged " +
			"tmux server this MOVES the workspace out from under a live agent (#1917 review)")
	}
	if inst.Tabs[0].tmux == nil {
		t.Fatal("the tab's tmux ref was cleared, so a retried archive has nothing to tear down")
	}
}

// TestTeardownTabs_WorktreeStateUnknown_SkipsFinalize is finding (3): a git
// cleanup cut off by its deadline leaves the workspace half-removed, so finalize
// must not run.
//
// finalize clears the tmux refs and the gitWorktree pointer. If it runs, a later
// retry finds no workspace, concludes there is nothing to do, "succeeds", and
// deletes the record — orphaning the partially-removed worktree forever, with
// nothing left pointing at it.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the core ran finalize unconditionally after
// handleWorktree, so the retain-the-tombstone fix bought nothing.
func TestTeardownTabs_WorktreeStateUnknown_SkipsFinalize(t *testing.T) {
	mode := &gateStubMode{closeState: stateKnown, worktreeState: stateUnknown}
	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})
	gw := &git.GitWorktree{}
	inst.gitWorktree = gw

	err := inst.teardownTabs(mode)

	if !mode.worktreeCalled {
		t.Fatal("the worktree action must run when the panes are confirmed dead")
	}
	if err == nil || !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("a cut-off worktree action must be reported so the caller keeps the record, got: %v", err)
	}
	if mode.finalizeCalled {
		t.Fatal("finalize ran after a worktree action that was cut off mid-flight: it clears the " +
			"refs a retry needs, so the retry sees no workspace, 'succeeds', and drops the record — " +
			"orphaning the half-removed worktree forever (#1917 review)")
	}
	// The retry must still find the workspace it has to finish removing.
	if inst.gitWorktree != gw {
		t.Fatal("the worktree pointer was cleared; the retry has lost the workspace")
	}
	if inst.Tabs[0].tmux == nil {
		t.Fatal("the tab's tmux ref was cleared; the retry has lost the session")
	}
}
