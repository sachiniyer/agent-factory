package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
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
	require.NoError(t, alpha.CloseTabByID(alpha.GetTabs()[2].ID))
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

// addSwapTabs equips inst with an agent tab (slot 0) plus the named shell tabs,
// giving each a "<prefix>-<name>" stable id — the same shape the swap tests above
// build inline. (paneTabKeys re-keys the agent slot by name regardless of its id,
// so the agent's id here is mutably-addressing only.)
func addSwapTabs(t *testing.T, inst *session.Instance, prefix string, names ...string) {
	t.Helper()
	for i, name := range names {
		kind := session.TabKindShell
		if i == 0 {
			kind = session.TabKindAgent
		}
		inst.AddTabForTest(name, kind)
		inst.GetTabs()[i].ID = prefix + "-" + name
	}
}

// TestSwapSameTitle_TabGoneWithOpenPane is the pane-open half of the #5298
// swap-path window: an open pane sits on the very tab the cursor rests on, and
// the daemon kills+recreates the session under the same title with a roster that
// DROPPED that tab (came back [agent, b] with "a" gone). reconcilePanesForTabs
// closes the pane and the swap's relayout re-reads the sidebar; on master that
// commits the re-sorted stale flat index via pushSelection, clobbers the active
// tab remapActiveTabForTabs just resolved and rewrites lastCursor* so the tail
// re-pin's replaced-session (by-name) lookup never runs — landing the workspace
// on the agent tab instead of the sibling. The pre-pin (SelectInstance ahead of
// the relayout) makes the sidebar sync a cursor move on intact lastCursor*
// evidence, so the active tab lands on the sibling "b" that took the vacated
// slot and the cursor lands on the replacement's instance row — the same landing
// the no-pane control (TestSwapSameTitleTabCursorSurvivesReplacementResort's
// shape) produces.
func TestSwapSameTitle_TabGoneWithOpenPane(t *testing.T) {
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
	addSwapTabs(t, stale, "stale", "agent", "a", "b")
	h.store.AddInstance(first)
	h.store.AddInstance(stale)
	h.store.AddInstance(last)
	h.sidebar.SelectInstance(stale)
	h.sidebar.SelectTabRow(stale.Title, 1)
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name)
	resizeHome(h, 200, 40)
	openTestPane(t, h, stale, 1) // an open pane on the viewed "a" tab
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The replacement drops the viewed "a" tab and re-sorts after "last".
	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = base.Add(3 * time.Hour)
	addSwapTabs(t, recreated, "fresh", "agent", "b")
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{
		first.ToInstanceData(), recreated.ToInstanceData(), last.ToInstanceData(),
	}))

	// The pane on the gone tab closed — proves the mid-swap relayout path ran.
	require.Equal(t, 0, h.store.NumOpenPanes(),
		"the pane on the dropped tab closed, so the swap's pane reconcile fired the sidebar-reading relayout")
	require.Same(t, recreated, h.sidebar.GetSelectedInstance(),
		"the snapshot's final assertion re-pins the replacement after it moves")
	assert.Equal(t, "b", recreated.GetTabs()[h.store.ActiveTab()].Name,
		"the active tab lands on the sibling 'b' that took the vacated slot, not the drifted stale slot (agent)")
	sel := h.sidebar.GetSelection()
	assert.False(t, sel.IsTab,
		"the cursor lands on the replacement's instance row — the viewed tab's name is gone from the roster, so the name re-pin finds no equivalent tab")
}

// TestSwapSameTitle_PartialRosterThenTabArrives pins the composed timeline
// behind #5620: the daemon creates the replacement's tabs one call at a time, so
// a snapshot poll can swap in a PARTIAL roster ([agent, b], "a" not yet created)
// and a later poll adds "a" in place. The swap lands on the sibling "b" that
// took the vacated slot (#5298), and the late arrival of "a" must not drag the
// active tab or the cursor away from where the user now is.
func TestSwapSameTitle_PartialRosterThenTabArrives(t *testing.T) {
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
	addSwapTabs(t, stale, "stale", "agent", "a", "b")
	h.store.AddInstance(first)
	h.store.AddInstance(stale)
	h.store.AddInstance(last)
	h.sidebar.SelectInstance(stale)
	h.sidebar.SelectTabRow(stale.Title, 1)
	resizeHome(h, 200, 40)
	openTestPane(t, h, stale, 1)

	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = base.Add(3 * time.Hour)
	addSwapTabs(t, recreated, "fresh", "agent", "b")
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))
	require.True(t, h.reconcileSnapshot([]session.InstanceData{
		first.ToInstanceData(), recreated.ToInstanceData(), last.ToInstanceData(),
	}))
	require.Equal(t, "b", recreated.GetTabs()[h.store.ActiveTab()].Name)

	// A later poll: "a" now exists on the daemon's row (same ID and CreatedAt, so
	// an in-place update, not another swap). It must arrive through the SNAPSHOT:
	// `recreated` is the live instance in the TUI projection now, so adding the tab
	// to it directly would put "a" in updateInstanceFromSnapshot's oldKeys and the
	// roster-change branch (active-tab remap + cursor sync) this test claims to pin
	// would never run (#5729). The daemon's row is a separate instance whose data
	// carries the new roster.
	require.Len(t, recreated.GetTabs(), 2, "precondition: the live row has not seen 'a' yet")
	// ReconcileTabsFromData is a no-op for an instance with no agent tmux session
	// or git worktree (a bare fake-backend row), so give the live row both — a
	// mock-exec session, never a real tmux — before the snapshot lands. "a" is a
	// web tab: it needs no tmux sibling to reconnect.
	cmdExec, _ := nameKeyedTmuxExec()
	recreated.SetTmuxSession(tmux.NewTmuxSessionWithDeps("dup-agent", "claude",
		tabHotkeysPty{t: t, cmdExec: cmdExec}, cmdExec))
	gw, err := git.NewGitWorktreeFromStorage(t.TempDir(), t.TempDir(), "dup", "dup", "", false, true)
	require.NoError(t, err)
	recreated.SetGitWorktreeForTest(gw)
	daemonRow := instanceWithFakeBackend(t, "dup")
	daemonRow.ID = recreated.ID
	daemonRow.CreatedAt = recreated.CreatedAt
	addSwapTabs(t, daemonRow, "fresh", "agent", "b")
	daemonRow.AddWebTabForTest("a", "http://localhost:3000")
	daemonRow.GetTabs()[2].ID = "fresh-a"
	h.reconcileSnapshot([]session.InstanceData{
		first.ToInstanceData(), daemonRow.ToInstanceData(), last.ToInstanceData(),
	})
	require.Same(t, recreated, h.sidebar.GetSelectedInstance(), "the row updated in place")
	require.Len(t, recreated.GetTabs(), 3, "the snapshot delivered 'a' as a genuinely new key")
	assert.Equal(t, "a", recreated.GetTabs()[2].Name)
	assert.Equal(t, "b", recreated.GetTabs()[h.store.ActiveTab()].Name,
		"a tab arriving after the swap must not move the active tab")
	// The swap removed the cursor's tab "a", so the cursor fell back to the
	// replacement's instance row; appending "a" later leaves the active "b" in the
	// same slot, so nothing re-syncs the cursor. It must stay on the instance row.
	sel := h.sidebar.GetSelection()
	assert.False(t, sel.IsTab, "a tab arriving after the swap must not pull the cursor onto a tab row")
}

// TestSwapSameTitle_TabReorderedWithOpenPane is the second pane-open repro: the
// replacement KEEPS the viewed tab but moves it ([agent, b, a], "a" 1→2, fresh
// ids) and re-sorts after "last". The pane re-binds and the swap's relayout
// re-reads the sidebar; on master pushSelection commits the re-sorted stale flat
// index, overwrites the active tab remap resolved, and rewrites lastCursor* to
// the replacement's id — so the tail re-pin's replacedSession branch reads FALSE
// and the by-name equivalent-tab lookup is skipped, landing both the cursor and
// active tab on slot 0 ("agent"). The pre-pin runs moveCursorToInstance on the
// corpse's intact evidence first, so both follow "a" by name to slot 2.
func TestSwapSameTitle_TabReorderedWithOpenPane(t *testing.T) {
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
	addSwapTabs(t, stale, "stale", "agent", "a", "b")
	h.store.AddInstance(first)
	h.store.AddInstance(stale)
	h.store.AddInstance(last)
	h.sidebar.SelectInstance(stale)
	h.sidebar.SelectTabRow(stale.Title, 1)
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name)
	resizeHome(h, 200, 40)
	openTestPane(t, h, stale, 1) // an open pane on the viewed "a" tab
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The replacement keeps "a" but moves it to slot 2, and re-sorts after "last".
	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = base.Add(3 * time.Hour)
	addSwapTabs(t, recreated, "fresh", "agent", "b", "a")
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{
		first.ToInstanceData(), recreated.ToInstanceData(), last.ToInstanceData(),
	}))

	// The pane re-bound to the equivalent "a" tab at its new slot — proves the
	// mid-swap relayout path ran (the pane reconcile re-binds, not closes).
	require.Equal(t, 1, h.store.NumOpenPanes(), "the pane followed its tab by name and stayed open")
	require.Same(t, recreated, h.sidebar.GetSelectedInstance())
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor follows the equivalent tab 'a' by name to slot 2")
	assert.Equal(t, 2, sel.TabIndex)
	assert.Equal(t, "a", recreated.GetTabs()[sel.TabIndex].Name)
	assert.Equal(t, 2, h.store.ActiveTab(),
		"the active tab follows the equivalent tab 'a' by name to slot 2, not the drifted slot 0 (agent)")
	assert.Equal(t, "a", recreated.GetTabs()[h.store.ActiveTab()].Name)
}

// TestSwapSameTitle_TabReorderedNoResortWithOpenPane is the no-re-sort variant of
// the pane-open repro: a single dup session whose replacement reorders the viewed
// tab ([agent, b, a], "a" 1→2, fresh ids) without moving the projection row. The
// tab reorder alone drifts the cursor's flat SUBTREE index (b now occupies a's
// old slot); on master the pane re-bind's relayout commits "b" via pushSelection
// and clobbers the name re-pin exactly as the re-sort case does. The pre-pin
// closes this window too — the drift source is irrelevant once the sidebar-reading
// relayout runs ahead of the re-pin.
func TestSwapSameTitle_TabReorderedNoResortWithOpenPane(t *testing.T) {
	h := newTestHome(t)
	stale := instanceWithFakeBackend(t, "dup")
	stale.ID = "stale-dup"
	addSwapTabs(t, stale, "stale", "agent", "a", "b")
	h.store.AddInstance(stale)
	h.sidebar.SetSelectedInstance(0)
	h.sidebar.SelectTabRow(stale.Title, 1)
	require.Equal(t, "a", stale.GetTabs()[h.store.ActiveTab()].Name)
	resizeHome(h, 200, 40)
	openTestPane(t, h, stale, 1) // an open pane on the viewed "a" tab
	require.Equal(t, 1, h.store.NumOpenPanes())

	// A single row does not re-sort-drift; the tab reorder ([a,b]→[b,a]) drifts
	// the cursor's subtree index on its own. Hold the CreatedAt fixed to isolate
	// that drift source from the cross-row re-sort the sibling tests exercise.
	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = stale.CreatedAt
	addSwapTabs(t, recreated, "fresh", "agent", "b", "a")
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{recreated.ToInstanceData()}))

	require.Equal(t, 1, h.store.NumOpenPanes(), "the pane followed its tab by name and stayed open")
	require.Same(t, recreated, h.sidebar.GetSelectedInstance())
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor follows the equivalent tab 'a' by name to slot 2")
	assert.Equal(t, 2, sel.TabIndex)
	assert.Equal(t, "a", recreated.GetTabs()[sel.TabIndex].Name)
	assert.Equal(t, 2, h.store.ActiveTab(),
		"the active tab follows the equivalent tab 'a' by name to slot 2, not the drifted slot 1 (b)")
	assert.Equal(t, "a", recreated.GetTabs()[h.store.ActiveTab()].Name)
}

// TestSwapSameTitle_HeaderCursorNotYankedByPrePin guards the production caveat:
// the swap's pre-pin (SelectInstance) is gated on the cursor actually resting on
// the swapped session's row. When the cursor rests on a section header, the swap
// must NOT yank it onto the swapped row — matching ReplaceInstance's deliberate
// non-asserting silent re-point. An open pane on a reordered tab still fires the
// mid-swap relayout, but the header cursor stays put (pushSelection early-returns
// on a header, and the gate keeps the pre-pin from asserting over it).
func TestSwapSameTitle_HeaderCursorNotYankedByPrePin(t *testing.T) {
	h := newTestHome(t)
	stale := instanceWithFakeBackend(t, "dup")
	stale.ID = "stale-dup"
	addSwapTabs(t, stale, "stale", "agent", "a", "b")
	other := instanceWithFakeBackend(t, "other")
	other.ID = "other-id"
	h.store.AddInstance(stale)
	h.store.AddInstance(other)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)

	// An open pane on dup's "a" tab so the swap's pane reconcile fires the
	// sidebar-reading relayout mid-swap.
	openTestPane(t, h, stale, 1)
	require.Equal(t, 1, h.store.NumOpenPanes())

	// Park the cursor on the Instances header the way a user does (a click), so
	// the cursor-derived selection goes nil while the store's display binding
	// stays sticky — the state the pre-pin's gate must NOT yank.
	clickZone(t, h, zones.TreeHeader)
	require.True(t, h.sidebar.GetSelection().IsHeader, "precondition: the cursor rests on the header")
	require.Nil(t, h.sidebar.GetSelectedInstance(), "precondition: a header cursor resolves to no instance")

	recreated := instanceWithFakeBackend(t, "dup")
	recreated.ID = "fresh-dup"
	recreated.CreatedAt = stale.CreatedAt
	addSwapTabs(t, recreated, "fresh", "agent", "b", "a")
	t.Cleanup(SetInstanceBuilderForTest(func(session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.reconcileSnapshot([]session.InstanceData{recreated.ToInstanceData(), other.ToInstanceData()}))

	postSel := h.sidebar.GetSelection()
	assert.True(t, postSel.IsHeader,
		"a header cursor must not be yanked onto the swapped row: the pre-pin is gated to the cursor resting on that session's row")
}
