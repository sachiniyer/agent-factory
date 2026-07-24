package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
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

// TestSidebarTreeRepinFollowsLegacyToNewSwap is the #2498 regression. A legacy
// pre-#1195 row carries no stable instance ID (ID ""), while restoreLocalTabs
// backfills non-empty IDs onto its tabs. When a kill/recreate swaps that row for
// a freshly built session (a real instance ID, freshly minted tab IDs), the
// sidebar cursor must follow its tab onto the equivalent slot of the
// replacement — the same name-domain rebind the pane reconcile applies for
// replacedSessionTabs (app/handle_panes.go), so the tree selection and the panes
// never disagree about which tab is "the same tab".
//
// The bug: moveCursorToInstance's replacedSession predicate required BOTH the
// stored and the target instance ID to be non-empty, so the legacy "" → new-id
// swap read as a same-session tab change. It then keyed on the stale backfilled
// tab ID, which no freshly minted tab carries, found nothing, and dropped the
// cursor to the instance row — losing the tab highlight. Against master this
// test lands on the instance row (sel.IsTab is false).
func TestSidebarTreeRepinFollowsLegacyToNewSwap(t *testing.T) {
	s := newTreeSidebar(t, 0)
	dir := t.TempDir()

	// A legacy record: no instance ID, tabs [agent, a, b] whose shell rows carry
	// the non-empty IDs restoreLocalTabs backfills onto pre-#1738 tabs.
	legacy, err := session.NewInstance(session.InstanceOptions{
		Title: "t-00", Path: dir, Program: "test",
	})
	require.NoError(t, err)
	legacy.ID = ""
	legacy.AddTabForTest("agent", session.TabKindAgent)
	legacy.AddTabForTest("a", session.TabKindShell)
	legacy.AddTabForTest("b", session.TabKindShell)
	legacy.GetTabs()[1].ID = "backfilled-a"
	legacy.GetTabs()[2].ID = "backfilled-b"
	addTestInstance(s, legacy)

	s.SetSelectedInstance(0)
	s.SelectTabRow(legacy.Title, 1) // rest the cursor on tab "a"
	require.Equal(t, "a", legacy.GetTabs()[s.GetSelection().TabIndex].Name,
		"precondition: the cursor rests on the legacy row's tab a")

	// The recreated session: a real minted instance ID, freshly minted tab IDs,
	// and a reordered roster so a slot-keyed re-pin would land on the wrong tab.
	recreated, err := session.NewInstance(session.InstanceOptions{
		Title: "t-00", Path: dir, Program: "test",
	})
	require.NoError(t, err)
	require.NotEmpty(t, recreated.ID, "precondition: a rebuilt session carries a stable ID")
	recreated.AddTabForTest("agent", session.TabKindAgent)
	recreated.AddTabForTest("b", session.TabKindShell)
	recreated.AddTabForTest("a", session.TabKindShell)
	recreated.GetTabs()[1].ID = "fresh-b"
	recreated.GetTabs()[2].ID = "fresh-a"

	// Swap the corpse for the live session, then fire the reconcile's #969 re-pin
	// assertion — exactly what swapInstanceFromSnapshot + reconcileSnapshot do.
	require.True(t, s.proj.ReplaceInstanceByTitle("t-00", recreated))
	s.proj.SelectInstance(recreated)

	sel := s.GetSelection()
	require.True(t, sel.IsTab,
		"the highlight follows the legacy→new swap onto a tab row, not the instance row (#2498)")
	got := recreated.GetTabs()[sel.TabIndex]
	assert.Equal(t, "a", got.Name,
		"the cursor lands on the equivalent replacement tab by name, matching the pane reconcile")
	assert.Equal(t, 2, sel.TabIndex,
		"tab a moved to slot 2 in the recreated roster; the re-pin follows its identity, not the stale ordinal")
}
