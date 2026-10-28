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
