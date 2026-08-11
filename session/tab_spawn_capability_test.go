package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1874: the sandbox runtimes (docker/ssh/hook) declare WorkspaceRemote — their
// workspace is off-box, so the daemon-side instance has no local git worktree
// (gitWorktree is assigned only in LocalBackend.Provision and the LOCAL restore
// branch of FromInstanceData). The tab paths that RUN A PROCESS require that
// worktree, so a TabManagement:true on these backends would advertise a
// capability no code path can service: the web menu would offer "New shell tab"
// and the daemon would reject the call.
//
// #3053 split that premise per kind. It is not true of every Add*Tab path: a web
// tab is a name and a URL, spawning nothing and reading nothing, so it works on
// any backend and is no longer governed by this bit. TabManagement stays false
// here because it now means what its name says — process-backed tabs — and those
// still cannot work off-box.
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

// TestSandboxInstanceTabSpawnIsPerKind pins the other half of the contract: what
// the Add*Tab paths can and cannot service on a sandbox instance, per KIND.
//
// Before #3053 this asserted that all four refuse, which was the defect — the
// web tab was refused for a worktree requirement it does not have. The
// assertions here are strictly stronger than that blanket form: each refusal
// must also name the requirement it is actually missing, so a message that
// degrades to "not supported on this backend" fails.
func TestSandboxInstanceTabSpawnIsPerKind(t *testing.T) {
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

			// Metadata only: nothing to spawn, nothing to read, so the MECHANICS
			// layer admits it even here. Whether the daemon can SERVE it off-box
			// is a policy question answered by Capabilities.RefuseTabKind, which
			// still refuses (#3062) — the two layers are deliberately separate so
			// the serving gap is not re-encoded as a fake worktree requirement.
			webTab, webErr := newInst().AddWebTab("http://localhost:3000", "")
			require.NoError(t, webErr,
				"AddWebTab must serve a worktree-less sandbox instance: a web tab needs no worktree (#3053)")
			require.NoError(t, b.Capabilities().RefuseTabKind(TabKindWeb, "https://example.com/app"),
				"an external HTTPS web tab is served off-box: nothing proxies it and it survives a restart (#3062)")
			require.Error(t, b.Capabilities().RefuseTabKind(TabKindWeb, "http://localhost:3000"),
				"a loopback target still needs a relay through the agent-server (#3062)")
			require.NotNil(t, webTab)
			assert.Equal(t, TabKindWeb, webTab.Kind)
			assert.Nil(t, webTab.tmux, "a web tab must hold no tmux session")

			// Needs the worktree to READ, which off-box cannot serve yet (#3054).
			_, vscodeErr := newInst().AddVSCodeTab("")
			require.Error(t, vscodeErr, "AddVSCodeTab must reject a worktree-less sandbox instance")
			assert.Contains(t, strings.ToLower(vscodeErr.Error()), "editor",
				"the vscode refusal must name the editor requirement, not a spawn it does not do")

			// Need a PTY in the local worktree.
			_, shellErr := newInst().AddShellTab()
			require.Error(t, shellErr, "AddShellTab must reject a worktree-less sandbox instance")
			assert.Contains(t, strings.ToLower(shellErr.Error()), "spawn",
				"the shell refusal must name the spawn it cannot do")

			_, procErr := newInst().AddProcessTab("echo hi", "")
			require.Error(t, procErr, "AddProcessTab must reject a worktree-less sandbox instance")
			assert.Contains(t, strings.ToLower(procErr.Error()), "spawn",
				"the process refusal must name the spawn it cannot do")
		})
	}
}

// A restore failure before remote Launch leaves started=true so the Lost retry
// loop can run, but the roster is still empty and PendingTabs are still staged.
// Web is metadata-only, yet it must not occupy slot 0 before Launch seeds the
// agent tab: that slot is unclosable and is the PTY stream's target.
func TestAddWebTab_RejectsFailedRestoreBeforeAgentTab(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{
		Title: "failed-restore", BackendType: "ssh", Status: Archived,
		PendingTabs: []TabData{{ID: "web-old", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/docs"}},
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(BeginRestore()))
	require.NoError(t, inst.Transition(AbortRestoreToLost()))
	require.True(t, inst.Started(), "Lost restore retries require started=true")
	require.Empty(t, inst.GetTabs(), "remote Launch has not seeded the agent tab")

	_, err = inst.AddWebTab("https://example.com/new", "new")
	require.Error(t, err, "a metadata tab must not take the reserved agent slot")
	assert.Contains(t, strings.ToLower(err.Error()), "agent tab")
	require.Empty(t, inst.GetTabs(), "a refused create must leave slot 0 empty for remote Launch")
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
