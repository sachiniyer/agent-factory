package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPanePreviewSuppressionFollowsTabIdentityAcrossReorder is the row-2
// regression from #1991. Dismissing a preview suppresses that exact
// original/target pair. If the target moves slots before the next preview tick,
// the suppression must follow its stable tab ID rather than accidentally
// re-opening the preview because the stored ordinal went stale.
func TestPanePreviewSuppressionFollowsTabIdentityAcrossReorder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		idless     bool
		backfillID bool
	}{
		{name: "stable ID"},
		{name: "legacy ID-less name fallback", idless: true},
		{name: "ID backfill after dismissal", idless: true, backfillID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, alpha := multiTabHome(t)
			pane := openTestPane(t, h, alpha, 1)

			h.sidebar.SelectTabRow(alpha.Title, 2)
			_ = h.selectionChanged()
			require.NotNil(t, h.panePreviewTxn, "precondition: tab 3 is being previewed")
			require.Equal(t, 2, h.panePreviewTxn.target.tab)
			targetName := alpha.GetTabs()[2].Name
			if tc.idless {
				alpha.GetTabs()[2].ID = ""
			} else {
				require.NotEmpty(t, alpha.GetTabs()[2].ID)
			}

			h.suppressActivePanePreview()
			h.cancelPanePreview(false)
			require.NotNil(t, h.panePreviewSuppression)
			require.Nil(t, h.panePreviewTxn)
			if tc.backfillID {
				alpha.GetTabs()[2].ID = "daemon-backfilled-target"
			}

			require.NoError(t, alpha.ReorderTab(2, 3))
			require.Equal(t, targetName, alpha.GetTabs()[3].Name,
				"precondition: the suppressed target moved from slot 2 to slot 3")
			_ = h.updatePanePreview(alpha, 3, true, false)

			assert.Nil(t, h.panePreviewTxn,
				"the next production preview pass must still suppress the same target after its reorder")
			assert.False(t, h.paneWindows[pane.ID()].Previewing())
		})
	}
}

// TestPanePreviewSuppressionDoesNotTransferAcrossStableIDReuse covers the
// close/recreate window after a stable target was dismissed. A legacy or
// older-daemon replacement can remain locally ID-less until a snapshot arrives;
// reusing the old name must not make it inherit the old tab's suppression.
func TestPanePreviewSuppressionDoesNotTransferAcrossStableIDReuse(t *testing.T) {
	h, alpha := multiTabHome(t)
	pane := openTestPane(t, h, alpha, 1)

	h.sidebar.SelectTabRow(alpha.Title, 2)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: shell-2 is being previewed")
	targetName := alpha.GetTabs()[2].Name
	require.NotEmpty(t, alpha.GetTabs()[2].ID, "precondition: the dismissed tab has stable identity")

	h.suppressActivePanePreview()
	h.cancelPanePreview(false)
	require.NotNil(t, h.panePreviewSuppression)
	require.NoError(t, alpha.CloseTab(2))
	alpha.AddTabForTest(targetName, session.TabKindShell)
	replacement := len(alpha.GetTabs()) - 1
	require.Empty(t, alpha.GetTabs()[replacement].ID,
		"precondition: the modeled legacy replacement is awaiting ID backfill")

	_ = h.updatePanePreview(alpha, replacement, true, false)

	require.NotNil(t, h.panePreviewTxn,
		"a distinct ID-less replacement must not inherit a stable tab's suppression")
	assert.Equal(t, replacement, h.panePreviewTxn.target.tab)
	assert.True(t, h.paneWindows[pane.ID()].Previewing())
}

// TestTUIViewStateRestorePrefersTabIDAcrossNameReuse is the row-3 regression
// from #1991. The payload is decoded from JSON so it compiles against the old
// schema: before TabID support the additive field is ignored and restore binds
// the pane and selection to whichever different tab now owns the stale name.
func TestTUIViewStateRestorePrefersTabIDAcrossNameReuse(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	inst := instanceWithFakeBackend(t, "alpha")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.GetTabs()[0].ID = "id-agent"
	inst.AddWebTabForTest("metrics", "http://localhost:3000")
	inst.AddWebTabForTest("shell", "http://localhost:3001")
	h.store.AddInstance(inst)

	targetID := inst.GetTabs()[1].ID
	staleNameOwnerID := inst.GetTabs()[2].ID
	require.NotEmpty(t, targetID)
	require.NotEqual(t, targetID, staleNameOwnerID)

	payload := map[string]any{
		"selected": map[string]any{
			"instance_id": inst.ID,
			"title":       inst.Title,
			"tab_id":      targetID,
			"tab_name":    "shell",
		},
		"active_tab": map[string]any{
			"instance_id": inst.ID,
			"title":       inst.Title,
			"tab_id":      targetID,
			"tab_name":    "shell",
		},
		"focus": map[string]any{
			"region":   tuiFocusRegionPane,
			"pane_key": tuiPaneKeyForInstance(inst, "shell"),
		},
		"open_panes": []map[string]any{{
			"key":         tuiPaneKeyForInstance(inst, "shell"),
			"instance_id": inst.ID,
			"title":       inst.Title,
			"tab_id":      targetID,
			"tab_name":    "shell",
			"focus_rank":  1,
		}},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var state config.TUIRepoViewState
	require.NoError(t, json.Unmarshal(raw, &state))

	require.Equal(t, 1, h.applyTUIViewState(state))
	require.Len(t, h.store.OpenPanes(), 1)
	pane := h.store.OpenPanes()[0]
	assert.Equal(t, targetID, inst.GetTabs()[pane.Tab()].ID,
		"the restored pane follows its saved tab ID, not the new owner of its stale name")
	assert.Equal(t, targetID, inst.GetTabs()[h.store.ActiveTab()].ID,
		"the restored selection follows its saved tab ID, not the new owner of its stale name")

	resizeHome(h, 120, 30)
	focused := h.focusedOpenPane()
	require.NotNil(t, focused, "the persisted pane focus survives the target tab's rename")
	assert.Equal(t, targetID, inst.GetTabs()[focused.Tab()].ID,
		"focus follows the ID-resolved pane, not the stale name embedded in its persisted key")
}

// TestTUIViewStateRestoreFallsBackToNameForUnresolvedTabID pins the deliberate
// best-effort exception: unlike a mutation RPC, view restoration should retain
// the user's layout when its saved ID no longer exists, so it falls back to the
// legacy name rather than dropping the pane and selection.
func TestTUIViewStateRestoreFallsBackToNameForUnresolvedTabID(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "alpha")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("shell", "http://localhost:3000")
	h.store.AddInstance(inst)

	state := config.TUIRepoViewState{
		Selected: &config.TUIStateTarget{
			InstanceID: inst.ID, Title: inst.Title, TabID: "gone-tab", TabName: "shell",
		},
		ActiveTab: &config.TUIStateTarget{
			InstanceID: inst.ID, Title: inst.Title, TabID: "gone-tab", TabName: "shell",
		},
		OpenPanes: []config.TUIStateOpenPane{{
			Key: tuiPaneKeyForInstance(inst, "shell"), InstanceID: inst.ID, Title: inst.Title,
			TabID: "gone-tab", TabName: "shell",
		}},
	}

	require.Equal(t, 1, h.applyTUIViewState(state))
	require.Len(t, h.store.OpenPanes(), 1)
	assert.Equal(t, "shell", inst.GetTabs()[h.store.OpenPanes()[0].Tab()].Name)
	assert.Equal(t, "shell", inst.GetTabs()[h.store.ActiveTab()].Name)
}

// TestTUIViewStateFocusSurvivesSwappedTabNames covers two restored panes whose
// stable IDs survive while their names swap. Translating the first pane's saved
// focus key must not let the translated key match and redirect focus to the
// second saved pane.
func TestTUIViewStateFocusSurvivesSwappedTabNames(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	inst := instanceWithFakeBackend(t, "alpha")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddWebTabForTest("b", "http://localhost:3000")
	inst.AddWebTabForTest("a", "http://localhost:3001")
	inst.GetTabs()[1].ID = "id-a"
	inst.GetTabs()[2].ID = "id-b"
	h.store.AddInstance(inst)

	state := config.TUIRepoViewState{
		Focus: &config.TUIStateFocus{
			Region: tuiFocusRegionPane, PaneKey: tuiPaneKeyForInstance(inst, "a"),
		},
		OpenPanes: []config.TUIStateOpenPane{
			{
				Key: tuiPaneKeyForInstance(inst, "a"), InstanceID: inst.ID, Title: inst.Title,
				TabID: "id-a", TabName: "a", FocusRank: 2,
			},
			{
				Key: tuiPaneKeyForInstance(inst, "b"), InstanceID: inst.ID, Title: inst.Title,
				TabID: "id-b", TabName: "b", FocusRank: 1,
			},
		},
	}

	require.Equal(t, 2, h.applyTUIViewState(state))
	resizeHome(h, 120, 30)
	focused := h.focusedOpenPane()
	require.NotNil(t, focused)
	assert.Equal(t, "id-a", inst.GetTabs()[focused.Tab()].ID,
		"focus must follow the originally saved key, not a translated key reused by another pane")
}

// TestSwapSameTitleActiveTabFollowsReplacementName is the row-4 regression
// from #1991. A replacement session has all-new tab IDs, so its equivalent tab
// is resolved in the replacement name domain, just like open panes already are.
func TestSwapSameTitleActiveTabFollowsReplacementName(t *testing.T) {
	h := newTestHome(t)
	stale := instanceWithFakeBackend(t, "dup")
	for i, name := range []string{"agent", "a", "b"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		stale.AddTabForTest(name, kind)
		stale.GetTabs()[i].ID = "stale-" + name
	}
	h.store.AddInstance(stale)
	h.sidebar.SetSelectedInstance(0)
	h.sidebar.SelectTabRow(stale.Title, 1)
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name)

	recreated := instanceWithFakeBackend(t, "dup")
	for i, name := range []string{"agent", "b", "a"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		recreated.AddTabForTest(name, kind)
		recreated.GetTabs()[i].ID = "fresh-" + name
	}
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{recreated.ToInstanceData()}))
	assert.Equal(t, "a", recreated.GetTabs()[h.store.ActiveTab()].Name,
		"the active tab follows its equivalent name across the replaced-session reorder")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	assert.Equal(t, "a", recreated.GetTabs()[sel.TabIndex].Name,
		"the cursor and active tab stay on the same replacement tab")
}

// TestSwapSameTitleTabCursorSurvivesReplacementResort is the stale-index edge
// behind row 4: ReplaceInstanceByTitle re-sorts by CreatedAt. The sidebar still
// carries the pre-sort projection index until the snapshot's final selection
// assertion rebuilds it, so synchronizing the cursor inside the swap can record
// the neighbor as lastCursor* and lose the selected replacement tab.
func TestSwapSameTitleTabCursorSurvivesReplacementResort(t *testing.T) {
	h := newTestHome(t)
	base := time.Now().Add(-4 * time.Hour)

	first := instanceWithFakeBackend(t, "first")
	first.ID = "first-id"
	first.CreatedAt = base
	stale := instanceWithFakeBackend(t, "dup")
	stale.ID = "stale-dup"
	stale.CreatedAt = base.Add(time.Hour)
	last := instanceWithFakeBackend(t, "last")
	last.ID = "last-id"
	last.CreatedAt = base.Add(2 * time.Hour)
	for i, name := range []string{"agent", "a", "b"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		stale.AddTabForTest(name, kind)
		stale.GetTabs()[i].ID = "stale-" + name
	}
	h.store.AddInstance(first)
	h.store.AddInstance(stale)
	h.store.AddInstance(last)
	h.sidebar.SelectInstance(stale)
	h.sidebar.SelectTabRow(stale.Title, 1)
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name)

	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = base.Add(3 * time.Hour) // moves after "last"
	for i, name := range []string{"agent", "b", "a"} {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		recreated.AddTabForTest(name, kind)
		recreated.GetTabs()[i].ID = "fresh-" + name
	}
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{
		first.ToInstanceData(), recreated.ToInstanceData(), last.ToInstanceData(),
	}))
	require.Same(t, recreated, h.sidebar.GetSelectedInstance(),
		"the snapshot's final assertion re-pins the replacement after it moves")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor must stay on its replacement tab row")
	assert.Equal(t, "a", recreated.GetTabs()[sel.TabIndex].Name)
	assert.Equal(t, "a", recreated.GetTabs()[h.store.ActiveTab()].Name)
}
