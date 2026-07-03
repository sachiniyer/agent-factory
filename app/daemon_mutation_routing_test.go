package app

import (
	"testing"

	"github.com/stretchr/testify/require"

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

	var gotTitle, gotRepo string
	restore := SetTabCreatorForTest(func(title, repoID string) (string, error) {
		gotTitle, gotRepo = title, repoID
		return nextShellTabName(inst.GetTabs()), nil
	})
	defer restore()

	_, _ = h.handleNewTab()

	require.Equal(t, inst.Title, gotTitle, "CreateTab must be called for the selected session")
	require.Equal(t, h.repoID, gotRepo, "CreateTab must be scoped to the TUI's repo")
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

	var gotTitle, gotRepo, gotTab string
	createRestore := SetTabCreatorForTest(func(title, repoID string) (string, error) {
		return nextShellTabName(inst.GetTabs()), nil
	})
	defer createRestore()
	closeRestore := SetTabCloserForTest(func(title, repoID, tabName string) error {
		gotTitle, gotRepo, gotTab = title, repoID, tabName
		return nil
	})
	defer closeRestore()

	_, _ = h.handleNewTab() // agent + shell + shell-2, active = 2
	require.Equal(t, 3, inst.TabCount())

	_, _ = h.handleCloseTab()

	require.Equal(t, inst.Title, gotTitle, "CloseTab must be called for the selected session")
	require.Equal(t, h.repoID, gotRepo, "CloseTab must be scoped to the TUI's repo")
	require.Equal(t, "shell-2", gotTab, "CloseTab must target the active tab by name")
	require.Equal(t, 2, inst.TabCount(), "the closed tab must be dropped locally")

	requireTUIInstancesEmpty(t, h)
}

// TestHandleCloseTab_AgentTabSkipsDaemon proves the agent-tab rule is enforced
// TUI-side without a daemon round-trip: `w` on tab 0 is a friendly no-op and the
// CloseTab RPC is never called.
func TestHandleCloseTab_AgentTabSkipsDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "agentskip")
	selectInstance(h, inst)
	h.contentPane.TabbedWindow().JumpToTab(0)

	called := false
	restore := SetTabCloserForTest(func(title, repoID, tabName string) error {
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
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, info: info})

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
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, branch: "feature-y", info: info})

	require.False(t, called, "a branch mismatch must not write PR info to the daemon")
	require.Nil(t, inst.GetPRInfo(), "a branch mismatch must not apply the badge")
}
