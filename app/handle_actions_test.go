package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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

// TestOpenCopyPRNoPRSurfacesMessage is the regression guard for #1170: p (open
// PR) and y (copy PR URL) on a session that has no PR yet must surface a brief,
// actionable message via the ErrBox rather than being a silent no-op. A nil
// selection (no session at all) stays silent — there is no session context to
// message about.
func TestOpenCopyPRNoPRSurfacesMessage(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)

	inst, err := session.NewInstance(session.InstanceOptions{Title: "no-pr", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Running)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	require.Nil(t, inst.GetPRInfo(), "precondition: session has no PR")

	// p (open PR)
	_, cmd := h.handleOpenPR()
	require.NotNil(t, cmd, "handleOpenPR must return a cmd that clears the message")
	require.Contains(t, h.errBox.String(), "no PR for this session yet")

	h.errBox.Clear()

	// y (copy PR URL)
	_, cmd = h.handleCopyPR()
	require.NotNil(t, cmd, "handleCopyPR must return a cmd that clears the message")
	require.Contains(t, h.errBox.String(), "no PR for this session yet")

	// Nil selection stays silent — no session context to report.
	h.errBox.Clear()
	h.store.RemoveInstanceByTitle("no-pr")
	require.Nil(t, h.sidebar.GetSelectedInstance(), "precondition: no session selected")
	_, cmd = h.handleOpenPR()
	require.Nil(t, cmd, "handleOpenPR on an empty selection must stay a silent no-op")
	require.Empty(t, strings.TrimSpace(h.errBox.String()))
}

func TestClipboardCommandForPlatform(t *testing.T) {
	lookPath := func(paths map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if path, ok := paths[name]; ok {
				return path, nil
			}
			return "", exec.ErrNotFound
		}
	}

	t.Run("darwin uses pbcopy", func(t *testing.T) {
		spec, err := clipboardCommandForPlatform("darwin", lookPath(map[string]string{"pbcopy": "/bin/pbcopy"}))
		require.NoError(t, err)
		require.Equal(t, clipboardCommandSpec{path: "/bin/pbcopy"}, spec)
	})

	t.Run("non-darwin prefers wl-copy", func(t *testing.T) {
		spec, err := clipboardCommandForPlatform("linux", lookPath(map[string]string{
			"wl-copy": "/bin/wl-copy",
			"xclip":   "/bin/xclip",
		}))
		require.NoError(t, err)
		require.Equal(t, clipboardCommandSpec{path: "/bin/wl-copy"}, spec)
	})

	t.Run("non-darwin falls back to xclip", func(t *testing.T) {
		spec, err := clipboardCommandForPlatform("linux", lookPath(map[string]string{"xclip": "/bin/xclip"}))
		require.NoError(t, err)
		require.Equal(t, clipboardCommandSpec{path: "/bin/xclip", args: []string{"-selection", "clipboard"}}, spec)
	})

	t.Run("missing tools are actionable", func(t *testing.T) {
		_, err := clipboardCommandForPlatform("linux", lookPath(nil))
		require.EqualError(t, err, "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS)")
	})
}

func TestRunClipboardCommandCapturesStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "cat >/dev/null; printf 'cannot open display\\n' >&2; exit 1")
	err := runClipboardCommand(cmd, "https://github.com/sachiniyer/agent-factory/pull/1")
	require.EqualError(t, err, "copy failed: cannot open display")
}

func TestHandleCopyPRFailureShowsReasonAndURL(t *testing.T) {
	const url = "https://github.com/sachiniyer/agent-factory/pull/1284"

	h := newTestHome(t)
	h.errBox.SetSize(500, 1)

	inst, err := session.NewInstance(session.InstanceOptions{Title: "has-pr", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Running)
	inst.SetPRInfo(&sessiongit.PRInfo{Number: 1284, Title: "clipboard", URL: url, State: "OPEN"})
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	prevCopy := copyToClipboard
	copyToClipboard = func(text string) error {
		require.Equal(t, url, text)
		return errors.New("copy failed: cannot open display")
	}
	t.Cleanup(func() { copyToClipboard = prevCopy })

	_, cmd := h.handleCopyPR()
	require.NotNil(t, cmd, "copy failure should return the normal clear-message command")

	rendered := h.errBox.String()
	require.Contains(t, rendered, "copy failed: cannot open display")
	require.Contains(t, rendered, "PR URL: "+url)
}
