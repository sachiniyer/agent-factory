package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestHandleNewTab_RoutesThroughDaemon_NoLocalSave proves the `t` mutation goes
// through the daemon CreateTab RPC and does NOT fall back to a local full-list
// save when the RPC succeeds (#960 PR 2). The daemon owns the persist; the TUI
// only reflects the tab locally via AttachShellTab.
func TestHandleNewTab_RoutesThroughDaemon_NoLocalSave(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "route-create")
	selectInstance(h, inst)

	var gotRequest daemon.CreateTabRequest
	restore := SetTabCreatorForTest(func(request daemon.CreateTabRequest) (string, string, error) {
		gotRequest = request
		return spawnDaemonTab(inst)
	})
	defer restore()

	_, _ = h.handleNewTab()

	require.Equal(t, daemon.CreateTabRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID, Shell: true}, gotRequest,
		"CreateTab must carry the selected session's stable identity")
	require.Equal(t, 3, inst.TabCount(), "the daemon-created tab must appear locally")

	// On the daemon-success path nothing is written to the TUI's storage — the
	// daemon is the single writer. The repo's instances file stays empty (the
	// instance was never added to TUI storage in this test).
	requireTUIInstancesEmpty(t, h)
}

// TestHandleCloseTab_RoutesThroughDaemon_NoLocalSave proves the `w` mutation
// goes through the daemon CloseTab RPC and drops the tab locally without a
// local full-list save when the RPC succeeds (#960 PR 2).
func TestHandleCloseTab_RoutesThroughDaemon_NoLocalSave(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "route-close")
	selectInstance(h, inst)

	var gotRequest daemon.CloseTabRequest
	createRestore := SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		return spawnDaemonTab(inst)
	})
	defer createRestore()
	closeRestore := SetTabCloserForTest(func(request daemon.CloseTabRequest) error {
		gotRequest = request
		return nil
	})
	defer closeRestore()

	_, _ = h.handleNewTab() // agent + shell + shell-2, active = 2
	require.Equal(t, 3, inst.TabCount())

	_, _ = h.handleCloseTab()

	require.Equal(t, daemon.CloseTabRequest{
		ID: inst.ID, Title: inst.Title, RepoID: h.repoID, TabName: "shell-2",
	}, gotRequest, "CloseTab must carry the selected session identity and active tab name")
	require.Equal(t, 2, inst.TabCount(), "the closed tab must be dropped locally")

	requireTUIInstancesEmpty(t, h)
}

// A tab name is reusable. If another client closes the intended tab and creates
// a new tab with the same name before the daemon resolves this request, a
// name-only close kills the replacement. The TUI already holds the stable tab
// ID and must carry it through the destructive RPC.
func TestHandleCloseTab_DoesNotTargetReusedTabName(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "route-close-reuse")
	selectInstance(h, inst)
	h.store.SetActiveTab(1)

	target := inst.GetTabs()[1]
	require.NotEmpty(t, target.ID)
	replacementID := "replacement-tab-id"
	daemonNameOwner := map[string]string{target.Name: replacementID}
	var killedID string
	restore := SetTabCloserForTest(func(request daemon.CloseTabRequest) error {
		if request.TabID != "" {
			killedID = request.TabID
		} else {
			killedID = daemonNameOwner[request.TabName]
		}
		return nil
	})
	defer restore()

	_, _ = h.handleCloseTab()
	require.Equal(t, target.ID, killedID,
		"the close must resolve the stable tab that was visible when the user acted")
	require.NotEqual(t, replacementID, killedID,
		"a reused tab name must not redirect the close to the replacement tab")
}

// TestHandleCloseTab_AgentTabSkipsDaemon proves the agent-tab rule is enforced
// TUI-side without a daemon round-trip: `w` on tab 0 is a friendly no-op and the
// CloseTab RPC is never called.
func TestHandleCloseTab_AgentTabSkipsDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "agentskip")
	selectInstance(h, inst)
	h.store.SetActiveTab(0)

	called := false
	restore := SetTabCloserForTest(func(daemon.CloseTabRequest) error {
		called = true
		return nil
	})
	defer restore()

	_, _ = h.handleCloseTab()

	require.False(t, called, "the agent tab must not round-trip to the daemon")
	require.Equal(t, 2, inst.TabCount(), "the agent tab must never be closed")
}

// TestPrInfoUpdatedMsg_RoutesWriteThroughDaemon proves the PR-info write goes
// through the daemon SetPRInfo RPC (the gh fetch stays TUI-side, #960 PR 2 §6)
// and applies the badge in-memory for instant UX.
func TestPrInfoUpdatedMsg_RoutesWriteThroughDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := newLoadingInstance(t, "pr-target")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var gotTitle, gotRepo string
	var gotInfo session.PRInfoData
	restore := SetPRInfoSetterForTest(func(title, repoID string, info session.PRInfoData) error {
		gotTitle, gotRepo, gotInfo = title, repoID, info
		return nil
	})
	defer restore()

	info := &sessiongit.PRInfo{Number: 42, Title: "add feature", URL: "https://x/42", State: "OPEN"}
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, repoID: h.repoID, info: info})

	require.Equal(t, inst.Title, gotTitle, "SetPRInfo must target the resolved session")
	require.Equal(t, h.repoID, gotRepo, "SetPRInfo must be scoped to the TUI's repo")
	require.Equal(t, 42, gotInfo.Number, "SetPRInfo must carry the fetched PR number")
	require.Equal(t, "add feature", gotInfo.Title)

	got := inst.GetPRInfo()
	require.NotNil(t, got, "the badge must be applied in-memory for instant UX")
	require.Equal(t, 42, got.Number)

	// The daemon owns the persist; the TUI writes nothing to instances.json.
	requireTUIInstancesEmpty(t, h)
}

// TestPrInfoUpdatedMsg_BranchMismatchSkipsDaemon proves the #921 branch guard
// still holds: when the captured fetch branch no longer matches the resolved
// instance's branch, neither the in-memory badge nor the daemon write is applied.
func TestPrInfoUpdatedMsg_BranchMismatchSkipsDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := newLoadingInstance(t, "pr-branch")
	inst.Branch = "feature-x"
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetPRInfoSetterForTest(func(title, repoID string, info session.PRInfoData) error {
		called = true
		return nil
	})
	defer restore()

	info := &sessiongit.PRInfo{Number: 99, Title: "wrong branch", State: "OPEN"}
	// The fetch was kicked off for a different branch than the instance now has.
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, branch: "feature-y", repoID: h.repoID, info: info})

	require.False(t, called, "a branch mismatch must not write PR info to the daemon")
	require.Nil(t, inst.GetPRInfo(), "a branch mismatch must not apply the badge")
}
