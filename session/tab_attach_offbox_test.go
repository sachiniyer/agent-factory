package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #6020: the daemon committed an off-box shell; the TUI's instant-display
// attach must reflect it as a tmux-less projection instead of refusing it with
// the local-only "not started" error, which reported a committed create as a
// failure and invited a retry.
func TestAttachOffBoxTabReflectsACommittedCreate(t *testing.T) {
	yes := true
	row, err := FromInstanceData(InstanceData{
		ID: "sbx", Title: "sandboxed", Path: "/srv/p", Program: "claude",
		BackendType: "docker", Liveness: LiveReady, TabManagement: &yes,
	})
	require.NoError(t, err)
	require.False(t, row.Started(), "premise: the snapshot row is inert")

	_, err = row.AttachShellTab("shell", "", "tab-1")
	require.Error(t, err, "premise: the local attach refuses an off-box row")

	before := len(row.GetTabs())
	tab, err := row.AttachOffBoxTab(TabKindShell, "shell", "tab-1")
	require.NoError(t, err)
	require.Equal(t, "tab-1", tab.ID)
	require.Equal(t, TabKindShell, tab.Kind)
	require.Len(t, row.GetTabs(), before+1)

	again, err := row.AttachOffBoxTab(TabKindShell, "shell", "tab-1")
	require.NoError(t, err)
	require.Same(t, tab, again, "a snapshot that won the race (or a repeat) joins the same tab")
	require.Len(t, row.GetTabs(), before+1, "one user action never shows two shells")

	_, err = row.AttachOffBoxTab(TabKindShell, "shell", "tab-2")
	require.Error(t, err, "a different id never hijacks a name another tab holds")
}
