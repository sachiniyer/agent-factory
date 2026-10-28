package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift is the regression
// guard for issue #716. For first-time attachers the attach is deferred until
// the attach help overlay is dismissed. The old code captured a method value
// (m.sidebar.Attach) that re-read the live selection at dismiss time, so a
// background refresh that drifted the selection onto a different instance while
// the overlay was open caused the attach to connect to the wrong instance.
//
// The fix captures the instance at Enter-press time (the synchronous moment the
// selection is provably current) and attaches to that captured instance. Since
// #1592 Phase 2 PR7 a local full-screen attach is a WS PTY proxy rather than a
// Backend.Attach() call, so the captured instance is observed via the title the
// attach call site passes to attachOverlayCallbackFn (instance.Title captured at
// Enter-press time). This test selects instance-a, presses Enter, drifts the
// selection to instance-b while the help overlay is open, then dismisses it and
// asserts the attach targeted instance-a.
func TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift(t *testing.T) {
	h := newTestHome(t)

	a := instanceWithFakeBackend(t, "instance-a")
	b := instanceWithFakeBackend(t, "instance-b")
	h.store.AddInstance(a)
	h.store.AddInstance(b)
	// User presses Enter on instance-a.
	h.sidebar.SetSelectedInstance(0)

	// The deferred attach records the title it targets. instance.Title is bound
	// into the attach closure at Enter-press time (#716), so a title of
	// "instance-b" here would mean the code re-read the drifted live selection.
	var attachedTitle string
	swapAttachOverlayCallbackFn(t, func(m *home, target sessionActionTarget, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		attachedTitle = target.title
		return nil
	})

	model, _ := h.handleEnter()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "first-time attach must show the help overlay")
	require.NotNil(t, h.textOverlay, "help overlay should be installed")

	// Background refresh drifts the selection onto instance-b while the overlay
	// is open.
	h.sidebar.SetSelectedInstance(1)
	require.Same(t, b, h.sidebar.GetSelectedInstance(),
		"precondition: selection must have drifted onto instance-b")

	// Dismissing the overlay schedules the attach transition; run it so the
	// deferred attach callback fires.
	_, cmd := h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnter})
	_ = runAttachTransitionCmd(t, h, cmd)

	require.Equal(t, "instance-a", attachedTitle,
		"attach must target the instance captured at Enter-press time, not the drifted selection")
}

func TestFirstRunAttachHelpEscCancelsAttach(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "alpha")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	attached := 0
	swapAttachOverlayCallbackFn(t, func(m *home, target sessionActionTarget, label, traceSuffix string, attach func() (chan struct{}, error)) tea.Cmd {
		attached++
		return nil
	})

	model, _ := h.handleAttach()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "first-time attach must show the help overlay")
	require.NotNil(t, h.textOverlay)

	_, cmd := h.handleHelpState(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "Esc must close the attach help overlay")
	require.False(t, h.attachTransitioning, "Esc cancel must not schedule the full-screen attach transition")
	runHermeticCmd(t, h, cmd, 0)
	require.Zero(t, attached, "Esc cancel must not invoke the attach callback")
}

// TestKillConfirmationWarning is the regression guard for issue #815. Kill
// force-removes the worktree (`git worktree remove -f`), bypassing git's own
// refusal to delete a dirty worktree, so the confirmation dialog's warning is
// the only safety gate. The old code only warned when `git status` succeeded
// AND reported changes; a failing status check (corrupted worktree, missing
// git metadata) silently produced no warning while deletion still proceeded.
// The check must fail closed: a status error yields a could-not-verify
// warning, not silence.
func TestKillConfirmationWarning(t *testing.T) {
	gitInit := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		cmd := exec.Command("git", "-C", dir, "init")
		require.NoError(t, cmd.Run(), "git init failed")
		return dir
	}

	t.Run("clean worktree produces no warning", func(t *testing.T) {
		dir := gitInit(t)
		require.Empty(t, killConfirmationWarning(dir))
	})

	t.Run("dirty worktree warns about uncommitted changes", func(t *testing.T) {
		dir := gitInit(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("data"), 0o644))
		warning := killConfirmationWarning(dir)
		require.Contains(t, warning, "uncommitted changes that will be lost")
		require.NotContains(t, warning, "Could not verify")
	})

	// #2101: `git status --porcelain` honours status.showUntrackedFiles, so a user
	// (or a global/main-repo config inherited by the worktree) that sets it to `no`
	// hid untracked files from the only safety check standing between D+y and
	// `git worktree remove -f`. The warning must not depend on user config.
	t.Run("untracked file with showUntrackedFiles=no still warns", func(t *testing.T) {
		dir := gitInit(t)
		cmd := exec.Command("git", "-C", dir, "config", "status.showUntrackedFiles", "no")
		require.NoError(t, cmd.Run(), "git config failed")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("data"), 0o644))
		warning := killConfirmationWarning(dir)
		require.Contains(t, warning, "uncommitted changes that will be lost",
			"warning should still be shown despite showUntrackedFiles=no")
		require.NotContains(t, warning, "Could not verify")
	})

	// The same hiding vector via the untracked file living in an untracked
	// subdirectory — the shape `-unormal` collapses to a single `?? dir/` entry.
	t.Run("untracked subdirectory with showUntrackedFiles=no still warns", func(t *testing.T) {
		dir := gitInit(t)
		cmd := exec.Command("git", "-C", dir, "config", "status.showUntrackedFiles", "no")
		require.NoError(t, cmd.Run(), "git config failed")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "notes", "deep"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes", "deep", "wip.txt"), []byte("data"), 0o644))
		require.Contains(t, killConfirmationWarning(dir), "uncommitted changes that will be lost")
	})

	t.Run("status check failure fails closed with could-not-verify warning", func(t *testing.T) {
		// A plain directory that is not a git repository makes `git status` fail.
		warning := killConfirmationWarning(t.TempDir())
		require.Contains(t, warning, "Could not verify worktree status")
		require.Contains(t, warning, "uncommitted changes that will be lost")
	})

	t.Run("nonexistent worktree path fails closed", func(t *testing.T) {
		warning := killConfirmationWarning(filepath.Join(t.TempDir(), "gone"))
		require.Contains(t, warning, "Could not verify worktree status")
	})
}
