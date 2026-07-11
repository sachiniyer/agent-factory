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

// TestHandleArchive_ConfirmationUsesEffectiveRestoreKey: the archive confirmation
// tells the user how to bring the session back, so it must name the effective
// RESTORE key (#1605), not the archive key — and it must track a [keys] rebind of
// `restore`.
func TestHandleArchive_ConfirmationUsesEffectiveRestoreKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string][]string
		wantKey   string
		notKey    string
	}{
		{name: "default", wantKey: "with r ", notKey: "with R "},
		{name: "pinned restore key", overrides: map[string][]string{"restore": {"R"}}, wantKey: "with R ", notKey: "with r "},
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

// TestHandleRestore_LostRowRestoresWithoutConfirmation: `r` on a Lost row
// restores it directly (no confirmation) via the dedicated restore verb (#1605).
func TestHandleRestore_LostRowRestoresWithoutConfirmation(t *testing.T) {
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

	model, cmd := h.handleRestore()
	h = model.(*home)

	require.Equal(t, stateDefault, h.state, "restoring a Lost session must not open a confirmation")
	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(), "Lost restore should show an in-flight restore state")
	require.NotNil(t, cmd, "Lost restore must dispatch the restore command")

	msg := cmd()
	require.Equal(t, "worker", gotTitle)
	done, ok := msg.(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.NoError(t, done.err)
}

// TestHandleArchive_RestingRowIsNoOp: after the #1605 clean break, `a` on an
// archived/lost/dead row does nothing — restore moved to `r`. It must not open a
// confirmation, mark a restore op, or dispatch anything.
func TestHandleArchive_RestingRowIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
	}{
		{"archived", session.Archived},
		{"lost", session.Lost},
		{"dead", session.Dead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", status)
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			model, cmd := h.handleArchive()
			h = model.(*home)

			require.Equal(t, stateDefault, h.state, "`a` on a resting row must not open any overlay")
			require.Equal(t, session.OpNone, inst.GetInFlightOp(), "`a` on a resting row must not start a restore")
			require.Nil(t, cmd, "`a` on a resting row must dispatch nothing")
		})
	}
}

// TestHandleRestore_LiveRowIsNoOp: `r` on a live row does nothing — archive stays
// on `a` (#1605).
func TestHandleRestore_LiveRowIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
	}{
		{"ready", session.Ready},
		{"running", session.Running},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", status)
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			model, cmd := h.handleRestore()
			h = model.(*home)

			require.Equal(t, stateDefault, h.state, "`r` on a live row must not open any overlay")
			require.Equal(t, session.OpNone, inst.GetInFlightOp(), "`r` on a live row must not start a restore")
			require.Nil(t, cmd, "`r` on a live row must dispatch nothing")
		})
	}
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
