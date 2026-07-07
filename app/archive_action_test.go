package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
)

// archiveActionInstance builds a started, mock-backed instance at the given
// status for the archive/restore action tests.
func archiveActionInstance(t *testing.T, title string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	return inst
}

// TestHandleArchive_LiveRowConfirms: archiving a live session opens the
// confirmation overlay (archive is significant — tmux down + worktree moved) and
// does not immediately dispatch the RPC.
func TestHandleArchive_LiveRowConfirms(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	prev := archiveSessionThroughDaemon
	archiveSessionThroughDaemon = func(string, string) (string, error) { called = true; return "", nil }
	defer func() { archiveSessionThroughDaemon = prev }()

	model, _ := h.handleArchive()
	h = model.(*home)

	require.Equal(t, stateConfirm, h.state, "archiving a live session must ask for confirmation")
	require.False(t, called, "the archive RPC must not fire before confirmation")
}

func TestHandleArchive_ConfirmationUsesEffectiveArchiveKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string][]string
		wantKey   string
		notKey    string
	}{
		{name: "default", wantKey: "with a.", notKey: "with A."},
		{name: "pinned old archive key", overrides: map[string][]string{"archive": {"A"}}, wantKey: "with A.", notKey: "with a."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, keys.ApplyOverrides(tc.overrides))
			t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", session.Ready)
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			model, _ := h.handleArchive()
			h = model.(*home)

			require.Equal(t, stateConfirm, h.state)
			require.NotNil(t, h.confirmationOverlay)
			rendered := strings.Join(strings.Fields(h.confirmationOverlay.Render()), " ")
			require.Contains(t, rendered, "Restore later")
			require.Contains(t, rendered, tc.wantKey)
			require.NotContains(t, rendered, tc.notKey)
		})
	}
}

func TestHandleArchive_LostRowRestoresWithoutConfirmation(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Lost)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var gotTitle string
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(title, repoID string) (string, error) {
		gotTitle = title
		return "/worktree/path", nil
	}
	defer func() { restoreSessionThroughDaemon = prev }()

	model, cmd := h.handleArchive()
	h = model.(*home)

	require.Equal(t, stateDefault, h.state, "restoring a Lost session must not open the archive confirmation")
	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(), "Lost restore should show an in-flight restore state")
	require.NotNil(t, cmd, "Lost restore must dispatch the restore command")

	msg := cmd()
	require.Equal(t, "worker", gotTitle)
	done, ok := msg.(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.NoError(t, done.err)
}

// TestArchiveInstanceCmd_CallsDaemon: the archive command invokes the daemon
// seam and reports completion.
func TestArchiveInstanceCmd_CallsDaemon(t *testing.T) {
	h := newTestHome(t)

	var gotTitle string
	prev := archiveSessionThroughDaemon
	archiveSessionThroughDaemon = func(title, repoID string) (string, error) {
		gotTitle = title
		return "/archive/path", nil
	}
	defer func() { archiveSessionThroughDaemon = prev }()

	msg := h.archiveInstanceCmd("worker")()
	require.Equal(t, "worker", gotTitle, "the archive command must call the daemon for the given title")
	done, ok := msg.(instanceArchivedMsg)
	require.True(t, ok, "the command must emit instanceArchivedMsg")
	require.NoError(t, done.err)
	require.Equal(t, "worker", done.title)
}

// TestArchiveInstanceCmd_SurfacesError: a daemon rejection is carried back as an
// error on the completion message (handled into the error box).
func TestArchiveInstanceCmd_SurfacesError(t *testing.T) {
	h := newTestHome(t)
	prev := archiveSessionThroughDaemon
	archiveSessionThroughDaemon = func(string, string) (string, error) {
		return "", errors.New("cannot archive in-place session")
	}
	defer func() { archiveSessionThroughDaemon = prev }()

	msg := h.archiveInstanceCmd("worker")()
	done := msg.(instanceArchivedMsg)
	require.Error(t, done.err)
}

// TestRestoreInstanceCmd_CallsDaemon: the restore command invokes the daemon
// seam and reports completion.
func TestRestoreInstanceCmd_CallsDaemon(t *testing.T) {
	h := newTestHome(t)

	var gotTitle string
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(title, repoID string) (string, error) {
		gotTitle = title
		return "/worktree/path", nil
	}
	defer func() { restoreSessionThroughDaemon = prev }()

	msg := h.restoreInstanceCmd("worker")()
	require.Equal(t, "worker", gotTitle)
	done, ok := msg.(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.NoError(t, done.err)
}

func TestHandleInstanceRestored_LostRowMarksLive(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Lost)
	inst.SetInFlightOpForTest(session.OpRestoring)
	h.store.AddInstance(inst)

	model, _ := h.handleInstanceRestored(instanceRestoredMsg{title: "worker"})
	h = model.(*home)

	require.Equal(t, session.Running, inst.GetStatus())
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
}
