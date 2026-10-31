package session

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1874: the sandbox runtimes (docker/ssh/sandbox/hook) declare WorkspaceRemote — their
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

// offBoxBackendType is a kind's persisted Backend.Type() discriminator, the
// key backendKindForType and newInertSandboxBackend route on. Hook persists as
// "remote". A kind missing here fails offBoxKinds, so a NEW off-box runtime
// cannot be registered without joining every test that enumerates the family.
var offBoxBackendType = map[BackendKind]string{
	BackendDocker:  "docker",
	BackendSSH:     "ssh",
	BackendSandbox: "sandbox",
	BackendHook:    "remote",
}

// offBoxKinds derives the off-box family from the runtime registry and its
// ProvisionsOffBox declaration rather than a hand-written list (W10.2). Four
// tests once enumerated docker/ssh/hook by hand and silently omitted sandbox.
func offBoxKinds() []BackendKind {
	var kinds []BackendKind
	for kind := range runtimeRegistry {
		if !kind.ProvisionsOffBox() {
			continue
		}
		if _, ok := offBoxBackendType[kind]; !ok {
			panic(fmt.Sprintf("off-box backend %q is registered but has no entry in offBoxBackendType; add it so the capability tests cover it", kind))
		}
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// sandboxBackends is the set of runtimes whose workspace is off-box, derived
// from the registry. Kept as one table so a NEW sandbox runtime is forced
// through the same contract.
func sandboxBackends() map[string]Backend {
	out := map[string]Backend{}
	for _, kind := range offBoxKinds() {
		out[string(kind)] = newInertSandboxBackend(offBoxBackendType[kind])
	}
	return out
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
// contract (#2013): an off-box backend's STATIC declaration must not claim it can
// swap its agent in place, AND its SwapAgent must actually refuse when there is no
// live sandbox to ask. A sandbox whose agent-server advertises the swap route
// lifts the bit per instance (W7.5, handoff_inplace_test.go). The bit and the behavior are
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
			require.NoError(t, b.Capabilities().RefuseTabKind(TabKindWeb, "http://localhost:3000"),
				"a loopback target is served from the workspace through the agent-server relay (W5.3)")
			require.NotNil(t, webTab)
			assert.Equal(t, TabKindWeb, webTab.Kind)
			assert.Nil(t, webTab.tmux, "a web tab must hold no tmux session")

			// The editor runs on the host (W5.4), so a worktree-less instance whose
			// runtime has one admits the tab as metadata.
			vscodeTab, vscodeErr := newInst().AddVSCodeTab("")
			require.NoError(t, vscodeErr, "AddVSCodeTab serves a worktree-less instance whose host runs the editor")
			assert.Equal(t, TabKindVSCode, vscodeTab.Kind)

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

// #6091: the remote editor is metadata too, and the same failed restore left it
// free to take slot 0, where it is unclosable and keeps remote Launch from seeding
// the agent tab. An editor never takes the agent slot; once the agent row exists it
// lands after it and closes like any other tab.
func TestAddVSCodeTab_NeverTakesTheAgentSlot(t *testing.T) {
	inst, err := FromInstanceData(InstanceData{Title: "failed-restore-editor", BackendType: "ssh", Status: Archived})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(BeginRestore()))
	require.NoError(t, inst.Transition(AbortRestoreToLost()))
	require.Empty(t, inst.GetTabs())

	_, err = inst.AddVSCodeTab("")
	require.Error(t, err, "an editor must not take the reserved agent slot")
	assert.Contains(t, strings.ToLower(err.Error()), "agent tab")
	require.Empty(t, inst.GetTabs(), "a refused create must leave slot 0 empty for remote Launch")

	// With the agent row seeded the editor is admitted after it, and closable.
	inst.mu.Lock()
	inst.Tabs = []*Tab{newRemoteAgentTab()}
	inst.mu.Unlock()
	tab, err := inst.AddVSCodeTab("")
	require.NoError(t, err)
	tabs := inst.GetTabs()
	require.Len(t, tabs, 2)
	assert.Equal(t, TabKindAgent, tabs[0].Kind)
	assert.Same(t, tab, tabs[1], "the editor sits after the agent slot")
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
