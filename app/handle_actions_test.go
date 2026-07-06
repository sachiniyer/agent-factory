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

// recordingAttachBackend wraps a FakeBackend and records the title of the
// instance whose Attach() is invoked, so tests can prove which instance the
// deferred attach actually connected to.
type recordingAttachBackend struct {
	*session.FakeBackend
	title string
	log   *[]string
}

func (b *recordingAttachBackend) Attach(*session.Instance) (chan struct{}, error) {
	*b.log = append(*b.log, b.title)
	ch := make(chan struct{})
	close(ch)
	return ch, nil
}

// TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift is the regression
// guard for issue #716. For first-time attachers the attach is deferred until
// the attach help overlay is dismissed. The old code captured a method value
// (m.sidebar.Attach) that re-read the live selection at dismiss time, so a
// background refresh that drifted the selection onto a different instance while
// the overlay was open caused the attach to connect to the wrong instance.
//
// The fix captures the instance at Enter-press time (the synchronous moment the
// selection is provably current) and attaches to that captured instance. This
// test selects instance-a, presses Enter, drifts the selection to instance-b
// while the help overlay is open, then dismisses it and asserts the attach
// targeted instance-a.
func TestHandleEnterAttachesCapturedInstanceAfterSelectionDrift(t *testing.T) {
	var attachLog []string
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		return &recordingAttachBackend{
			FakeBackend: session.NewFakeBackend(),
			title:       opts.Title,
			log:         &attachLog,
		}, nil
	})
	defer restore()

	h := newTestHome(t)

	a, err := session.NewInstance(session.InstanceOptions{Title: "instance-a", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	a.SetStatus(session.Running)
	b, err := session.NewInstance(session.InstanceOptions{Title: "instance-b", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	b.SetStatus(session.Running)

	h.store.AddInstance(a)
	h.store.AddInstance(b)
	// User presses Enter on instance-a.
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.handleEnter()
	h = model.(*home)
	require.Equal(t, stateHelp, h.state, "first-time attach must show the help overlay")
	require.NotNil(t, h.textOverlay, "help overlay should be installed")

	// Background refresh drifts the selection onto instance-b while the overlay
	// is open.
	h.sidebar.SetSelectedInstance(1)
	require.Same(t, b, h.sidebar.GetSelectedInstance(),
		"precondition: selection must have drifted onto instance-b")

	// Dismissing the overlay runs the deferred attach callback.
	_, _ = h.handleHelpState(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, []string{"instance-a"}, attachLog,
		"attach must target the instance captured at Enter-press time, not the drifted selection")
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
// PR) and P (copy PR URL) on a session that has no PR yet must surface a brief,
// actionable message via the ErrBox rather than being a silent no-op. A nil
// selection (no session at all) stays silent — there is no session context to
// message about.
func TestOpenCopyPRNoPRSurfacesMessage(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)

	inst, err := session.NewInstance(session.InstanceOptions{Title: "no-pr", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetStatus(session.Running)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	require.Nil(t, inst.GetPRInfo(), "precondition: session has no PR")

	// p (open PR)
	_, cmd := h.handleOpenPR()
	require.NotNil(t, cmd, "handleOpenPR must return a cmd that clears the message")
	require.Contains(t, h.errBox.String(), "no PR for this session yet")

	h.errBox.Clear()

	// P (copy PR URL)
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
	inst.SetStatus(session.Running)
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
