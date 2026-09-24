package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Tab reorder (session.tab.reorder): the TUI's </> pair routes the focused tab
// through the daemon's ReorderTab RPC — the same path the web's drag reorder
// and `af sessions tab-reorder` take — carrying the stable session + tab ids,
// then applies the daemon's resolved index so panes and the tree selection
// follow their tab across the permutation rather than landing on whatever slid
// into the slot.
// ----------------------------------------------------------------------------

// recordReorderTab swaps in a reorderTabThroughDaemon seam that records the
// requests it was asked to send and answers success — the resolved name/index
// echoed back, exactly as the daemon reports what it committed.
func recordReorderTab(t *testing.T) *[]daemon.ReorderTabRequest {
	t.Helper()
	var reqs []daemon.ReorderTabRequest
	t.Cleanup(SetTabReordererForTest(func(req daemon.ReorderTabRequest) (daemon.ReorderTabResponse, error) {
		reqs = append(reqs, req)
		return daemon.ReorderTabResponse{Name: req.TabName, Index: req.NewIndex}, nil
	}))
	return &reqs
}

// TestMoveTab_RightPermutesAndCarriesSelection is the headline: `>` on the
// tree's active tab sends one daemon request addressed by stable ids, permutes
// the projection to the daemon's answer, and re-points the tree selection at
// the moved tab's NEW slot — the selection is an identity, not an ordinal.
func TestMoveTab_RightPermutesAndCarriesSelection(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1) // shell (id-shell)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Len(t, *reqs, 1)
	req := (*reqs)[0]
	assert.Equal(t, alpha.ID, req.ID, "the session is addressed by its stable id")
	assert.Equal(t, "id-shell", req.TabID, "the tab is addressed by its stable id")
	assert.Equal(t, "shell", req.TabName, "the name rides along as the legacy fallback")
	assert.Equal(t, 2, req.NewIndex)

	require.Equal(t, []string{"agent", "shell-2", "shell", "shell-3"}, tabNames(alpha),
		"the projection applies the daemon's resolved index")
	require.Equal(t, 2, h.store.ActiveTab(),
		"the tree selection moves WITH its tab instead of staying on the ordinal")
	require.Equal(t, "shell", alpha.GetTabs()[h.store.ActiveTab()].Name)
}

// TestMoveTab_LeftPermutes mirrors the rightward case so both directions are
// exercised through the dispatched keymap, not just the handler.
func TestMoveTab_LeftPermutes(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(3) // shell-3 (id-shell3)
	reqs := recordReorderTab(t)

	pressNav(t, h, "<")

	require.Len(t, *reqs, 1)
	assert.Equal(t, "id-shell3", (*reqs)[0].TabID)
	assert.Equal(t, 2, (*reqs)[0].NewIndex)
	require.Equal(t, []string{"agent", "shell", "shell-3", "shell-2"}, tabNames(alpha))
	require.Equal(t, 2, h.store.ActiveTab())
	require.Equal(t, "shell-3", alpha.GetTabs()[h.store.ActiveTab()].Name)
}

// TestMoveTab_PaneFocusedMovesThePaneTab is the #1884 rule applied to reorder:
// with a pane focused, </> acts on the tab ON SCREEN — the pane's binding —
// not the tree's active tab. The pane follows its tab to the new slot; the
// tree's selection keeps pointing at its own unmoved tab.
func TestMoveTab_PaneFocusedMovesThePaneTab(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(1) // tree on shell (slot 1)

	pane := openTestPane(t, h, alpha, 1)
	_, _ = h.handleTabJump(4) // pane jumps to shell-3 (slot 3); tree stays on 1
	require.Equal(t, 3, pane.Tab())
	require.Equal(t, 1, h.store.ActiveTab())
	reqs := recordReorderTab(t)

	pressNav(t, h, "<")

	require.Len(t, *reqs, 1, "the move must target the focused pane's tab")
	assert.Equal(t, "id-shell3", (*reqs)[0].TabID,
		"</> must move shell-3 — the tab on screen — not the tree's shell")
	assert.Equal(t, 2, (*reqs)[0].NewIndex)

	require.Equal(t, []string{"agent", "shell", "shell-3", "shell-2"}, tabNames(alpha))
	require.Equal(t, 2, pane.Tab(),
		"the pane re-binds to its own tab's new slot (stable id, not the ordinal)")
	require.Equal(t, "shell-3", alpha.GetTabs()[pane.Tab()].Name)
	assert.Equal(t, 1, h.store.ActiveTab(),
		"the tree was on shell (slot 1), which did not move — its active tab must be untouched")
}

// TestMoveTab_AgentTabRefuses pins the invariant TUI-side: </> on the agent tab
// notices instead of round-tripping, and the roster never moves.
func TestMoveTab_AgentTabRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(0)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Empty(t, *reqs, "the agent tab never reaches the daemon")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "pinned to the first slot")
}

// TestMoveTab_LeftOntoAgentSlotRefuses is the other half of the pin: the tab at
// slot 1 cannot move left past the agent tab.
func TestMoveTab_LeftOntoAgentSlotRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1)
	reqs := recordReorderTab(t)

	pressNav(t, h, "<")

	require.Empty(t, *reqs)
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "pinned to the first slot")
}

// TestMoveTab_AtLastSlotRefuses covers the far boundary: `>` on the last tab is
// a notice, not a silent swallow.
func TestMoveTab_AtLastSlotRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(3)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Empty(t, *reqs)
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "already at the last position")
}

// TestMoveTab_DaemonErrorLeavesRoster is the fail-closed half: a daemon refusal
// (stale id, another writer won) surfaces the error and the projection is
// untouched — the daemon is the only writer.
func TestMoveTab_DaemonErrorLeavesRoster(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1)
	t.Cleanup(SetTabReordererForTest(func(daemon.ReorderTabRequest) (daemon.ReorderTabResponse, error) {
		return daemon.ReorderTabResponse{}, fmt.Errorf("session %q tab id %q: tab is gone", alpha.Title, "id-shell")
	}))

	pressNav(t, h, ">")

	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha),
		"a refused reorder must not touch the projection")
	require.Equal(t, 1, h.store.ActiveTab())
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "tab is gone")
}

// TestMoveTab_RemoteRefuses keeps `</>` behind the same TabManagement gate as
// `t`/`w`: the snapshot's ReconcileTabsFromData skips off-box backends, so a
// move there could diverge from a second client's roster with nothing to heal
// it. The refusal is a local notice — the daemon itself would allow it.
func TestMoveTab_RemoteRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	alpha.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	require.False(t, alpha.Capabilities().TabManagement, "precondition: remote backend has no tab management")
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Empty(t, *reqs, "an off-box roster never reaches the daemon")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "off-box")
}

// TestMoveTab_ArchivedRefuses pins the roster-freeze invariant: the daemon
// rejects every tab mutation on an archived session to keep the roster intact
// for restore, so the TUI refuses before the request can hit the wire.
func TestMoveTab_ArchivedRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	alpha.SetArchived()
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Empty(t, *reqs, "an archived session's roster is frozen")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "archived")
}

// TestMoveTab_ArchivedRowCursorRefuses is the reorder half of
// TestCloseTab_ArchivedRowCursorDoesNotRetarget: on an archived row the
// sidebar cursor resolves to the ARCHIVED instance while the store's sticky
// selection stays on a LIVE one — so an unguarded `>` would reorder a session
// the user is not looking at. The footer advertises no </> there, and the
// press must refuse on what the cursor names.
func TestMoveTab_ArchivedRowCursorRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(2)

	archived := instanceWithFakeBackend(t, "old")
	archived.AddTabForTest("agent", session.TabKindAgent)
	archived.AddTabForTest("old-shell", session.TabKindShell)
	archived.AddTabForTest("old-shell-2", session.TabKindShell)
	archived.SetArchived()
	h.store.AddInstance(archived)
	_ = h.selectionChanged()

	h.sidebar.SelectInstance(archived)
	require.Equal(t, archived, h.sidebar.GetSelectedInstance(),
		"precondition: the cursor resolves to the ARCHIVED instance")
	require.Equal(t, alpha, h.store.GetSelectedInstance(),
		"precondition: the store's display selection stays sticky on the LIVE instance")

	h.focusRegion(layout.RegionTree)
	reqs := recordReorderTab(t)

	pressNav(t, h, ">")

	require.Empty(t, *reqs, "a move must never reach the daemon from an archived row")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha),
		"the hidden live session's roster must not permute")
	h.errBox.SetSize(200, 1)
	assert.Contains(t, h.errBox.String(), "archived")
}

// TestMoveTab_AutomationsFocusInert is the captive-focus half: while the
// Automations rail owns focus, </> must not fall through to the global
// dispatcher and reorder a session the user is not looking at — the rail's
// footer does not advertise the pair. Driven through handleKeyPress so the
// assertion covers the real dispatch path, not just the swallow helper.
func TestMoveTab_AutomationsFocusInert(t *testing.T) {
	h, alpha := multiTabHome(t)
	// The rail's Automations section exists only when a task populates it, and
	// ring.Focus refuses a hidden region — so the relayout that un-hides it
	// must run BEFORE focusRegion asks for it.
	h.store.SetTasks([]task.Task{{ID: "t1", Name: "watch"}})
	h.relayout()
	h.store.SetActiveTab(1)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused(), "precondition: automations rail holds focus")
	reqs := recordReorderTab(t)

	for _, k := range []string{">", "<"} {
		_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
	require.Empty(t, *reqs, "no move may fire while the Automations rail owns focus")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "the key must not move focus either")
}

// TestMoveTab_OtherPanesFollowTheirTabs is the roster-wide identity check: a
// move must rebind EVERY open pane to wherever its own tab landed, keyed by
// stable id — never leave a pane showing the tab that slid into its old slot.
func TestMoveTab_OtherPanesFollowTheirTabs(t *testing.T) {
	h, alpha := multiTabHome(t)
	paneA := openTestPane(t, h, alpha, 1) // shell
	paneB := openTestPane(t, h, alpha, 3) // shell-3, holds focus
	h.store.SetActiveTab(1)
	reqs := recordReorderTab(t)

	pressNav(t, h, "<") // focused paneB's tab: shell-3 3→2

	require.Len(t, *reqs, 1)
	require.Equal(t, []string{"agent", "shell", "shell-3", "shell-2"}, tabNames(alpha))
	require.Equal(t, 2, paneB.Tab(), "the moved pane follows shell-3 to slot 2")
	require.Equal(t, 1, paneA.Tab(), "the unmoved pane stays on shell at slot 1")
	require.Equal(t, "shell-3", alpha.GetTabs()[paneB.Tab()].Name)
	require.Equal(t, "shell", alpha.GetTabs()[paneA.Tab()].Name)
}
