package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSidebarTreeRepinTracksTabIdentityAcrossReorder is the row-1 regression
// from #1991. The display selection remains sticky while the cursor is parked
// on the section header; a later SelectInstance assertion must re-pin the cursor
// to the tab it last occupied, even if an out-of-band reorder moved that tab to
// another slot in the meantime.
func TestSidebarTreeRepinTracksTabIdentityAcrossReorder(t *testing.T) {
	s := newTreeSidebar(t, 1)
	inst := s.proj.GetInstances()[0]
	inst.AddWebTabForTest("a", "http://localhost:3000")
	inst.AddWebTabForTest("b", "http://localhost:3001")
	s.SetSelectedInstance(0)

	s.SelectTabRow(inst.Title, 2)
	wantID := inst.GetTabs()[2].ID
	require.NotEmpty(t, wantID)
	require.Equal(t, wantID, inst.GetTabs()[s.GetSelection().TabIndex].ID)

	// Clicking the Instances header twice collapses then re-expands the section,
	// leaving the cursor on the header while its previous instance/tab selection
	// remains sticky — a real public navigation path to the latent state.
	s.ClickHeader()
	s.ClickHeader()
	require.True(t, s.GetSelection().IsHeader,
		"precondition: the cursor is off the tab rows while the instance selection stays sticky")
	require.Same(t, inst, s.proj.GetSelectedInstance())

	require.NoError(t, inst.ReorderTab(2, 3))
	require.Equal(t, wantID, inst.GetTabs()[3].ID, "precondition: tab a moved from slot 2 to slot 3")
	s.proj.SelectInstance(inst)

	sel := s.GetSelection()
	require.True(t, sel.IsTab, "the instance re-pin restores a tab row")
	assert.Equal(t, wantID, inst.GetTabs()[sel.TabIndex].ID,
		"the re-pin follows the last cursor tab by stable identity, not its stale slot")
}

// TestSidebarTreeRepinDoesNotHijackReusedTabName is the destructive complement:
// within one session, a stable ID that left the roster means the old tab is
// gone. A newly created tab may reuse its name, but the re-pin must fall back to
// the instance row rather than silently selecting that imposter.
func TestSidebarTreeRepinDoesNotHijackReusedTabName(t *testing.T) {
	s := newTreeSidebar(t, 1)
	inst := s.proj.GetInstances()[0]
	inst.AddWebTabForTest("a", "http://localhost:3000")
	inst.AddWebTabForTest("b", "http://localhost:3001")
	s.SetSelectedInstance(0)
	s.SelectTabRow(inst.Title, 2)
	oldID := inst.GetTabs()[2].ID
	require.NotEmpty(t, oldID)

	s.ClickHeader()
	s.ClickHeader()
	require.True(t, s.GetSelection().IsHeader)

	require.NoError(t, inst.DropClosedTab(2))
	inst.AddWebTabForTest("a", "http://localhost:3002")
	require.NotEqual(t, oldID, inst.GetTabs()[3].ID,
		"precondition: the freed name belongs to a newly created tab")
	s.proj.SelectInstance(inst)

	sel := s.GetSelection()
	assert.False(t, sel.IsTab,
		"an unresolved stable ID falls back to the instance row, never a reused name or stale slot")
	assert.Equal(t, 0, sel.ItemIndex)
}

// TestSidebarTreeRepinAgentUsesNameAcrossIDHealing protects the agent-tab
// exception shared with pane reconciliation: its locally minted ID may be
// replaced by the daemon's authoritative ID in place, while the positional
// singleton itself remains the same tab.
func TestSidebarTreeRepinAgentUsesNameAcrossIDHealing(t *testing.T) {
	s := newTreeSidebar(t, 1)
	inst := s.proj.GetInstances()[0]
	inst.GetTabs()[0].ID = "local-agent-id"
	s.SetSelectedInstance(0)
	s.SelectTabRow(inst.Title, 0)

	s.ClickHeader()
	s.ClickHeader()
	require.True(t, s.GetSelection().IsHeader)

	inst.GetTabs()[0].ID = "daemon-agent-id"
	s.proj.SelectInstance(inst)

	sel := s.GetSelection()
	require.True(t, sel.IsTab, "the re-pin keeps the agent tab across its ID heal")
	assert.Equal(t, 0, sel.TabIndex)
}
