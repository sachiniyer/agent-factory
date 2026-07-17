package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TREE's selection across a tab-set change, driven through the REAL daemon
// snapshot reconcile. The pane twin of this lives in pane_tab_reconcile_test.go;
// this is the other half of the same rule, because store.ActiveTab is a SECOND
// piece of tab state the reconcile must carry across a permutation.
//
// A selection is an IDENTITY ("the tab I am looking at"), not a position. The
// reorder path (#1813) permutes the roster with no local action at all, so an
// index-keyed selection silently becomes a DIFFERENT tab under the user — the
// #1906 class arriving through the door the reorder opened. Every assertion here
// therefore reads the tab's ID out of the roster at the selected index; asserting
// the index itself would pass while the bug is live.

// TestTree_SnapshotTabReorderKeepsSelectionOnSameTab: another client reorders
// the roster, and the tree's selected tab must FOLLOW its own tab to the new
// slot rather than stay on the ordinal and select whatever slid into it.
func TestTree_SnapshotTabReorderKeepsSelectionOnSameTab(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "selorder")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, err := inst.AddWebTab("http://localhost:3000", "a")
	require.NoError(t, err)
	_, err = inst.AddWebTab("http://localhost:3001", "b")
	require.NoError(t, err)

	// The user selects tab "a" through the real tree-focus 1-9 jump.
	_, _ = h.handleTabJump(2)
	require.Equal(t, 1, h.store.ActiveTab())
	wantID := inst.GetTabs()[1].ID
	require.NotEmpty(t, wantID)
	require.Equal(t, "a", inst.GetTabs()[h.store.ActiveTab()].Name)

	// The daemon reports the roster reordered to [agent, b, a].
	data := inst.ToInstanceData()
	require.Equal(t, []string{"a", "b"}, []string{data.Tabs[1].Name, data.Tabs[2].Name})
	data.Tabs[1], data.Tabs[2] = data.Tabs[2], data.Tabs[1]

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))
	require.Equal(t, []string{"b", "a"},
		[]string{inst.GetTabs()[1].Name, inst.GetTabs()[2].Name}, "the local roster permuted")

	active := h.store.ActiveTab()
	require.GreaterOrEqual(t, active, 0, "the selection stays in range")
	require.Less(t, active, inst.TabCount(), "the selection stays in range")
	assert.Equal(t, wantID, inst.GetTabs()[active].ID,
		"the selection follows its OWN tab across the reorder")
	assert.Equal(t, "a", inst.GetTabs()[active].Name)
}

// TestTree_SnapshotTabReorderMovesCursorWithSelection is the same rule stated at
// the sidebar cursor, and it is not redundant: the cursor's tab row is keyed by
// SLOT (rowIdentity/SidebarItem.TabIndex), so a reorder leaves it on the ordinal
// too. That matters twice over — the tree highlight would point at a different
// tab than the one the active tab resolves to, and the next pushSelection reads
// the cursor's slot back into store.ActiveTab, which would silently CLOBBER the
// remap the reconcile just made.
func TestTree_SnapshotTabReorderMovesCursorWithSelection(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "selcursor")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, err := inst.AddWebTab("http://localhost:3000", "a")
	require.NoError(t, err)
	_, err = inst.AddWebTab("http://localhost:3001", "b")
	require.NoError(t, err)

	// Park the cursor ON tab "a"'s row — the state where the cursor is a second
	// index-keyed holder of "which tab am I looking at".
	h.store.SetActiveTab(1)
	h.sidebar.SelectTabRow(inst.Title, 1)
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 1, h.sidebar.GetSelection().TabIndex)
	wantID := inst.GetTabs()[1].ID

	data := inst.ToInstanceData()
	data.Tabs[1], data.Tabs[2] = data.Tabs[2], data.Tabs[1]
	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor stays on a tab row")
	require.GreaterOrEqual(t, sel.TabIndex, 0)
	require.Less(t, sel.TabIndex, inst.TabCount())
	assert.Equal(t, wantID, inst.GetTabs()[sel.TabIndex].ID,
		"the cursor follows its own tab row across the reorder")
	// And the two halves agree: reading the selection back must not clobber it.
	assert.Equal(t, wantID, inst.GetTabs()[h.store.ActiveTab()].ID,
		"pushSelection re-reads the cursor, so the active tab must survive the read")
}
