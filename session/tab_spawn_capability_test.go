package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1874: the sandbox runtimes (docker/ssh/hook) declare WorkspaceRemote — their
// workspace is off-box, so the daemon-side instance has no local git worktree
// (gitWorktree is assigned only in LocalBackend.Provision and the LOCAL restore
// branch of FromInstanceData). Every Add*Tab path requires that worktree, so a
// TabManagement:true on these backends would advertise a capability no code path
// can service: the web menu would offer "New shell tab"/"Open in VS Code" and
// the daemon would reject the call.
//
// These tests pin the capability to what the implementation can actually do, so
// the advertisement and the behavior cannot drift apart again. When tab creation
// is routed through the agent-server (issue #1874 option 1), the assertions flip
// to "the tab is created" rather than being deleted.

// sandboxBackends is the set of runtimes whose workspace is off-box. Kept as one
// table so a NEW sandbox runtime is forced through the same contract.
func sandboxBackends() map[string]Backend {
	return map[string]Backend{
		"docker": &dockerBackend{},
		"ssh":    &sshBackend{},
		"hook":   &HookBackend{},
	}
}

// TestSandboxBackendsDoNotAdvertiseTabManagement is the contract: a backend
// whose workspace is off-box must not claim user-managed tabs while every
// Add*Tab path requires a local worktree.
func TestSandboxBackendsDoNotAdvertiseTabManagement(t *testing.T) {
	for name, b := range sandboxBackends() {
		t.Run(name, func(t *testing.T) {
			caps := b.Capabilities()
			require.Equal(t, WorkspaceRemote, caps.Workspace,
				"%s is expected to be an off-box runtime", name)
			assert.False(t, caps.TabManagement,
				"%s advertises TabManagement but every Add*Tab path requires a local worktree (#1874)", name)
		})
	}
}

// TestSandboxBackendsDoNotAdvertiseHandoff is the handoff half of the same
// contract (#2013): an off-box backend must not claim it can swap its agent in
// place, AND its SwapAgent must actually refuse. The bit and the behavior are
// asserted together for the same reason as TabManagement above — a capability
// that lies in either direction is worse than one that is simply false.
func TestSandboxBackendsDoNotAdvertiseHandoff(t *testing.T) {
	for name, b := range sandboxBackends() {
		t.Run(name, func(t *testing.T) {
			assert.False(t, b.Capabilities().Handoff,
				"%s advertises Handoff, but swapping the agent inside a provisioned sandbox is a re-launch its recover path does not do (#2013)", name)

			err := b.SwapAgent(&Instance{Title: "sandbox-inst", backend: b, started: true, Tabs: []*Tab{newRemoteAgentTab()}}, AgentSwapPlan{})
			require.Error(t, err, "%s must refuse a swap outright rather than half-perform it", name)
			assert.ErrorIs(t, err, ErrHandoffUnsupported,
				"%s must refuse with the typed sentinel so clients can render the restriction instead of matching prose", name)
		})
	}
}

// TestSandboxInstanceTabSpawnRejected pins the other half of the contract: the
// Add*Tab paths really are unable to service a sandbox instance. If this ever
// starts passing, the capability above should be flipped to true in the same
// change — that is what makes the fix decidable rather than a guess.
func TestSandboxInstanceTabSpawnRejected(t *testing.T) {
	for name, b := range sandboxBackends() {
		t.Run(name, func(t *testing.T) {
			// A started sandbox instance, exactly as Launch leaves it: started,
			// one remote agent tab, and no local worktree or tmux.
			newInst := func() *Instance {
				return &Instance{
					Title:   "sandbox-inst",
					backend: b,
					started: true,
					Tabs:    []*Tab{newRemoteAgentTab()},
				}
			}

			_, shellErr := newInst().AddShellTab()
			assert.Error(t, shellErr, "AddShellTab must reject a worktree-less sandbox instance")

			_, webErr := newInst().AddWebTab("http://localhost:3000", "")
			assert.Error(t, webErr, "AddWebTab must reject a worktree-less sandbox instance")

			_, vscodeErr := newInst().AddVSCodeTab("")
			assert.Error(t, vscodeErr, "AddVSCodeTab must reject a worktree-less sandbox instance")

			_, procErr := newInst().AddProcessTab("echo hi", "")
			assert.Error(t, procErr, "AddProcessTab must reject a worktree-less sandbox instance")
		})
	}
}

// TestSandboxTabSpawnErrorIsNotMisleading covers the copy half of #1874. The old
// message was "cannot add a tab to an instance that is not started", which is
// false on its face: the instance IS started. An error a user cannot act on is
// the bug, so assert the message names the real reason (no local workspace)
// rather than a state that is not the cause.
func TestSandboxTabSpawnErrorIsNotMisleading(t *testing.T) {
	i := &Instance{
		Title:   "sandbox-inst",
		backend: &dockerBackend{},
		started: true,
		Tabs:    []*Tab{newRemoteAgentTab()},
	}
	_, err := i.AddShellTab()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not started",
		"the instance IS started; the message must name the real reason (#1874)")
}
