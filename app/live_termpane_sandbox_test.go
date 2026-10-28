package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/session"
)

// sandboxFakeBackend is the remote double typed as a provisioned sandbox, so
// StreamsViaAgentServer sees it the way it sees a real docker/ssh/sandbox row.
// remoteFakeBackend alone is typed "remote", the hook backend.
type sandboxFakeBackend struct{ remoteFakeBackend }

func (sandboxFakeBackend) Type() string { return "sandbox" }

func sandboxInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst := instanceWithFakeBackend(t, title)
	inst.SetBackend(sandboxFakeBackend{remoteFakeBackend{session.NewFakeBackend()}})
	inst.SetStatusForTest(session.Running)
	inst.AddTabWithIDForTest("tab-agent-id", "agent", session.TabKindAgent)
	inst.AddTabWithIDForTest("tab-shell-id", "shell", session.TabKindShell)
	require.Equal(t, session.WorkspaceRemote, inst.Capabilities().Workspace)
	return inst
}

// A sandbox tab has no local tmux session, so it binds by its stable tab id —
// the same identity the daemon streams it by (#5571 W4.8).
func TestLiveSessionNameAdmitsSandboxTabsByStableID(t *testing.T) {
	inst := sandboxInstance(t, "sbx")
	assert.Equal(t, "tab-agent-id", liveSessionName(inst, 0))
	assert.Equal(t, "tab-shell-id", liveSessionName(inst, 1))
}

func TestLiveSessionNameStillRefusesWhatCannotStream(t *testing.T) {
	// The remote hook backend attaches through its terminal_cmd, not the daemon WS.
	hook := instanceWithFakeBackend(t, "hook")
	hook.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	hook.SetStatusForTest(session.Running)
	hook.AddTabWithIDForTest("hook-tab", "agent", session.TabKindAgent)
	assert.Empty(t, liveSessionName(hook, 0), "a hook-backed remote row keeps the full-screen attach")

	// A tab with no stable id yet has nothing to bind a stream to.
	noID := sandboxInstance(t, "noid")
	noID.AddTabWithIDForTest("", "legacy", session.TabKindShell)
	assert.Empty(t, liveSessionName(noID, 2))

	// Out of range and not started stay refused.
	assert.Empty(t, liveSessionName(sandboxInstance(t, "range"), 9))
	unstarted := sandboxInstance(t, "unstarted")
	unstarted.SetStartedForTest(false)
	assert.Empty(t, liveSessionName(unstarted, 0))
}

// A visible sandbox pane gets a live attachment addressed by the session id and
// the tab's stable id, and its capture polling stops — it streams (W4.8).
func TestSyncLiveTermPaneBindsSandboxPaneByTabID(t *testing.T) {
	h := newTestHome(t)
	inst := sandboxInstance(t, "sbx-pane")
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 1)
	var gotTabIDs []string
	fakes, addrs := stubLiveTermFactory(t)
	orig := newLiveTermPaneFn
	newLiveTermPaneFn = func(s apiclient.StreamSession, tabID string, tab, w, hh int) liveTermAttachment {
		gotTabIDs = append(gotTabIDs, tabID)
		return orig(s, tabID, tab, w, hh)
	}
	t.Cleanup(func() { newLiveTermPaneFn = orig })

	h.syncLiveTermPane()

	require.Len(t, *fakes, 1, "a sandbox pane must bind a live attachment")
	assert.Equal(t, streamAddress(inst, h.repoID), (*addrs)[0])
	assert.Equal(t, []string{"tab-shell-id"}, gotTabIDs, "the stream is addressed by the tab's stable id")
	p := h.focusedOpenPane()
	require.NotNil(t, p)
	assert.True(t, h.paneWindows[p.ID()].HasLive())
}

// Metadata-only tabs own no PTY: a live attach would blank the pane that
// otherwise shows the tab's placeholder, and Enter keeps its local behavior
// (liveSessionName is also the Enter eligibility predicate) (#5825).
func TestLiveSessionNameRefusesSandboxTabsWithoutPTY(t *testing.T) {
	inst := sandboxInstance(t, "sbx-meta")
	inst.AddTabWithIDForTest("tab-web-id", "web", session.TabKindWeb)
	inst.AddTabWithIDForTest("tab-docs-id", "docs", session.TabKindDocs)
	inst.AddTabWithIDForTest("tab-proc-id", "proc", session.TabKindProcess)
	assert.Empty(t, liveSessionName(inst, 2), "web tab")
	assert.Empty(t, liveSessionName(inst, 3), "docs tab")
	assert.Equal(t, "tab-proc-id", liveSessionName(inst, 4), "process tab still streams")
	assert.Equal(t, "tab-shell-id", liveSessionName(inst, 1))
}
