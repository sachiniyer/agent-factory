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

// TestTree_SnapshotLegacyToNewSwapFollowsTabHighlight is the #2498 regression at
// the daemon-reconcile entry point. A legacy pre-#1195 row carries no instance ID
// (ID ""), while restoreLocalTabs backfills non-empty IDs onto its tabs. When a
// same-title kill/recreate swaps it for a freshly built session (a real instance
// ID, freshly minted tab IDs), BOTH tab bindings the reconcile carries — the
// store's active tab AND the sidebar's tree cursor — must follow the equivalent
// tab by name, the same replacedSessionTabs rule the panes use.
//
// Before the fix the tree cursor's own replacedSession predicate required both
// instance IDs to be non-empty, so it missed the legacy "" → new-id swap, keyed
// the re-pin on the stale backfilled tab id (which no fresh tab carries), and
// dropped the cursor to the instance row. The active-tab remap — keyed
// unconditionally on the name domain — was already correct, so the two disagreed.
// Against master sel.IsTab is false.
func TestTree_SnapshotLegacyToNewSwapFollowsTabHighlight(t *testing.T) {
	h := newTestHome(t)

	// A legacy record: no instance ID, tabs [agent, a, b] whose rows carry the
	// non-empty IDs restoreLocalTabs backfills onto pre-#1738 tabs.
	stale := instanceWithFakeBackend(t, "legacy-sess")
	stale.ID = ""
	for i, name := range []string{"agent", "a", "b"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		stale.AddTabForTest(name, kind)
		stale.GetTabs()[i].ID = "backfilled-" + name
	}
	selectInstance(h, stale)
	resizeHome(h, 200, 40)
	h.sidebar.SelectTabRow(stale.Title, 1) // rest the cursor on tab "a"
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name,
		"precondition: the cursor and active tab rest on the legacy row's tab a")

	// The recreated session: a real instance ID, freshly minted tab IDs, and a
	// reordered roster so a slot-keyed re-pin would land on the wrong tab.
	recreated := instanceWithFakeBackend(t, "legacy-sess")
	recreated.ID = "new-session-id"
	for i, name := range []string{"agent", "b", "a"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		recreated.AddTabForTest(name, kind)
		recreated.GetTabs()[i].ID = "fresh-" + name
	}
	// swapInstanceFromSnapshot builds the live replacement through this seam.
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{recreated.ToInstanceData()}),
		"a same-title kill/recreate is a visible change")

	// The active tab (already correct on master) and the sidebar cursor (the fix)
	// must agree: both land on the equivalent replacement tab a at its new slot.
	assert.Equal(t, "a", recreated.GetTabs()[h.store.ActiveTab()].Name,
		"the store's active tab follows the replacement tab by name")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab,
		"the sidebar cursor follows the legacy→new swap onto a tab row, not the instance row (#2498)")
	assert.Equal(t, "a", recreated.GetTabs()[sel.TabIndex].Name,
		"the cursor lands on tab a, matching the active-tab remap and the pane reconcile")
}
