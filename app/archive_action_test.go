package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

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
	inst.SetStatus(status)
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
	prev := restoreArchivedThroughDaemon
	restoreArchivedThroughDaemon = func(title, repoID string) (string, error) {
		gotTitle = title
		return "/worktree/path", nil
	}
	defer func() { restoreArchivedThroughDaemon = prev }()

	msg := h.restoreInstanceCmd("worker")()
	require.Equal(t, "worker", gotTitle)
	done, ok := msg.(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.NoError(t, done.err)
}
