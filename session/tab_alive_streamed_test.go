package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5874: a streamed sandbox tab has no local tmux binding, so its liveness
// comes from the projected agent-server roster, not from tmux.
func TestTabAlive_StreamedSandboxTabsUseRoster(t *testing.T) {
	inst := &Instance{}
	inst.SetBackend(&failingStartBackend{FakeBackend: NewFakeBackend(), typ: "sandbox"})
	inst.AddTabWithIDForTest("a", "agent", TabKindAgent)
	inst.AddTabWithIDForTest("s", "shell", TabKindShell)
	inst.AddTabWithIDForTest("w", "web", TabKindWeb)
	inst.AddTabWithIDForTest("p", "proc", TabKindProcess)
	inst.Tabs[3].Exit = &TabExit{}

	require.True(t, inst.TabAlive(0))
	require.True(t, inst.TabAlive(1), "streamed shell tab is alive, so scroll mode can enter")
	require.False(t, inst.TabAlive(2), "web tab has no PTY")
	require.False(t, inst.TabAlive(3), "a process tab that recorded an exit is gone")
	require.False(t, inst.TabAlive(9), "out of range")

	// A hook-backed remote row has no streamed PTY here: unchanged.
	hook := &Instance{}
	hook.SetBackend(&failingStartBackend{FakeBackend: NewFakeBackend(), typ: "remote"})
	hook.AddTabWithIDForTest("s", "shell", TabKindShell)
	require.False(t, hook.TabAlive(0))
}

// #5953: a client row built from a snapshot has no live agent-server to read the
// tab-API advertisement from, so the daemon's projected verdict lifts
// TabManagement and the TUI lists and spawns remote shell/process tabs.
func TestTabManagementProjectsToSnapshotRows(t *testing.T) {
	daemonRow := &Instance{Title: "d", backend: offBoxNoTabsBackend{&failingStartBackend{FakeBackend: NewFakeBackend(), typ: "sandbox"}}}
	d := daemonRow.ToInstanceData()
	require.NotNil(t, d.TabManagement, "always projected so false survives the wire")
	require.False(t, *d.TabManagement, "no live advertisement: the static floor")
	require.Nil(t, d.ForStorage().TabManagement, "derived, never persisted")

	yes := true
	d.TabManagement = &yes
	d.Liveness, d.BackendType = LiveReady, "docker"
	row, err := FromInstanceData(d)
	require.NoError(t, err)
	require.Equal(t, WorkspaceRemote, row.Capabilities().Workspace)
	require.True(t, row.Capabilities().TabManagement, "the projection lifts the inert row")

	no := false
	require.True(t, row.ReconcileTabManagementSnapshot(&no))
	require.False(t, row.Capabilities().TabManagement)
	require.False(t, row.ReconcileTabManagementSnapshot(&no))
	require.False(t, row.ReconcileTabManagementSnapshot(nil), "an older daemon leaves the row alone")
}

// offBoxNoTabsBackend is an off-box runtime whose static declaration refuses
// process tabs (the floor a live advertisement lifts).
type offBoxNoTabsBackend struct{ *failingStartBackend }

func (offBoxNoTabsBackend) Capabilities() Capabilities {
	return Capabilities{Workspace: WorkspaceRemote}
}
