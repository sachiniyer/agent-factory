package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Multi-tab pane cycling (#1884/#1885/#1886/#1905): the TUI pane layer keys on
// the FOCUSED PANE's binding and the STABLE TAB ID, mirroring the web #1855 /
// #1862 / #1738 guarantees. These drive the exact tab-cycling play-test
// sequences the three issues were found by.
// ----------------------------------------------------------------------------

// multiTabHome builds a home with one fake-backend instance "alpha" carrying
// four real tab slots — agent, shell, shell-2, shell-3 — each with an explicit
// stable id, selected and sized for panes. It is the 4-tab shape the tab-cycling
// play-test used (agent + 3× t).
func multiTabHome(t *testing.T) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	alpha := instanceWithFakeBackend(t, "alpha")
	alpha.AddTabForTest("agent", session.TabKindAgent)
	alpha.AddTabForTest("shell", session.TabKindShell)
	alpha.AddTabForTest("shell-2", session.TabKindShell)
	alpha.AddTabForTest("shell-3", session.TabKindShell)
	// AddTabForTest leaves the id empty; the play-test tabs carry stable ids
	// (#1738), which the id-keyed reconcile depends on. Set them explicitly via
	// the GetTabs pointers (the returned slice copies the pointers, so the
	// mutation lands on the real tabs).
	for i, tab := range alpha.GetTabs() {
		tab.ID = tabIDForSlot(i)
	}
	h.store.AddInstance(alpha)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)
	return h, alpha
}

func tabIDForSlot(i int) string {
	return []string{"id-agent", "id-shell", "id-shell2", "id-shell3"}[i]
}

// TestPaneJump_StaysPut_NoPreviewRepaint is the #1885 headline: a pane-focused
// number-key jump is a COMMIT — pressing 4 lands on tab 4 and STAYS there, even
// though the tree cursor still points at a different tab and a background preview
// tick fires. Before the fix, the trailing selectionChanged repainted a
// tree-cursor preview over the pane, so "press 4" showed tab 2.
func TestPaneJump_StaysPut_NoPreviewRepaint(t *testing.T) {
	h, alpha := multiTabHome(t)

	// The tree's active tab is a SPECIFIC tab that disagrees with where the pane
	// will land — the normal state right after a tree-focus jump, and the state
	// the repaint needs (a same-instance non-tab-specific selection is already
	// cancelled by the #1289 guard, so it would hide the bug).
	h.store.SetActiveTab(1) // tree active tab = shell (slot 1)
	h.menu.SetActiveTab(1)

	// Open alpha's shell pane and focus it, then jump it elsewhere.
	pane := openTestPane(t, h, alpha, 1)
	require.Equal(t, pane, h.focusedOpenPane(), "precondition: the pane holds focus")

	// Pane-focused jump to tab 4 (shell-3, slot 3). The tree's active tab stays 1.
	_, _ = h.handleTabJump(4)
	require.Equal(t, 1, h.store.ActiveTab(),
		"precondition: the pane jump does not move the tree's active tab (#1884), so preview and commit diverge")

	require.Equal(t, 3, pane.Tab(), "the jump commits: the pane binds tab 4 (slot 3)")
	require.Nil(t, h.panePreviewTxn,
		"the tree's active tab still points at tab 1, but the committed jump to tab 4 must NOT be repainted by a preview (#1885)")
	w := h.paneWindows[pane.ID()]
	require.NotNil(t, w)
	require.False(t, w.Previewing(),
		"no transient preview may sit over the committed tab-4 binding")

	require.True(t, h.paneJumpIntentPinned(pane.ID()),
		"the explicit jump pins the pane's intent for the current selection epoch")

	// A background preview tick (the idle 100ms refresh) re-derives the preview
	// from the tree selection. The pinned intent must keep it from clobbering the
	// jump.
	_, _ = h.Update(previewTickMsg{})
	require.Equal(t, 3, pane.Tab(), "a background tick must not move the committed pane off tab 4")
	require.Nil(t, h.panePreviewTxn, "a background tick must not repaint the committed pane")

	// A genuine tree navigation bumps the selection epoch, staling the pin so
	// tree-cursor previews resume — the commit is scoped to the current selection,
	// not forever.
	epochBefore := h.selectionEpoch
	pressNav(t, h, "j") // move the sidebar cursor to a different row
	require.Greater(t, h.selectionEpoch, epochBefore, "a genuine navigation advances the selection epoch")
	require.False(t, h.paneJumpIntentPinned(pane.ID()), "the pin stales once the user navigates")
}

// TestCloseTab_ActsOnFocusedPaneTab is the #1884 destructive corollary (Repro
// B): with the tree's active tab a *closable* tab and the focused pane jumped to
// a DIFFERENT tab, w must close the tab the user is LOOKING at (the pane's),
// never the tree's. Before the fix, w read store.ActiveTab() and silently
// destroyed the tree's tab.
func TestCloseTab_ActsOnFocusedPaneTab(t *testing.T) {
	h, alpha := multiTabHome(t)

	// Tree active tab = shell (slot 1) — a closable tab, the destructive
	// precondition.
	h.store.SetActiveTab(1)
	h.menu.SetActiveTab(1)

	// Open a pane and jump it to shell-3 (slot 3) — the tab on screen.
	pane := openTestPane(t, h, alpha, 1)
	_, _ = h.handleTabJump(4)
	require.Equal(t, 3, pane.Tab(), "precondition: the focused pane shows tab 4 (shell-3)")
	require.Equal(t, 1, h.store.ActiveTab(), "precondition: the tree's active tab is still shell (slot 1)")

	closerCalls := recordCloseTab(t, h)

	_, _ = h.handleCloseTab()

	require.Equal(t, []string{"shell-3"}, *closerCalls,
		"w must close the FOCUSED pane's tab (shell-3), not the tree's active tab (shell)")
}

// TestCloseTab_FocusedAgentPaneRefuses is the #1884 Repro A: the focused pane is
// on the agent tab while the tree's active tab is a closable tab. w must refuse
// (the agent tab can't be closed) rather than silently closing the tree's tab.
func TestCloseTab_FocusedAgentPaneRefuses(t *testing.T) {
	h, alpha := multiTabHome(t)

	// Tree active tab = shell-2 (slot 2), a closable tab.
	h.store.SetActiveTab(2)
	h.menu.SetActiveTab(2)

	// Focused pane on the agent tab (slot 0) — the tab actually on screen.
	pane := openTestPane(t, h, alpha, 0)
	require.Equal(t, 0, pane.Tab())

	closerCalls := recordCloseTab(t, h)

	_, _ = h.handleCloseTab()

	require.Empty(t, *closerCalls, "no tab may be closed — the focused pane is the agent tab")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "agent tab can't be closed",
		"w on a focused agent pane must show the agent-tab message, not close the tree's tab")
	require.Equal(t, 4, alpha.TabCount(), "the tree's shell-2 tab must survive untouched")
}

// TestCloseTab_TreeFocusUnchanged pins that with NO pane focused, w still closes
// the tree's active tab — the existing behavior must be preserved.
func TestCloseTab_TreeFocusUnchanged(t *testing.T) {
	h, _ := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(2) // shell-2
	h.menu.SetActiveTab(2)
	require.Nil(t, h.focusedOpenPane(), "precondition: no pane holds focus")

	closerCalls := recordCloseTab(t, h)

	_, _ = h.handleCloseTab()

	require.Equal(t, []string{"shell-2"}, *closerCalls,
		"with tree focus, w closes the tree's active tab, as before")
}

// recordCloseTab swaps in a closeTabThroughDaemon seam that records the tab
// names it was asked to close (and drops the tab locally so the reconcile runs),
// returning a pointer to the recorded names.
func recordCloseTab(t *testing.T, h *home) *[]string {
	t.Helper()
	var names []string
	t.Cleanup(SetTabCloserForTest(func(title, repoID, tabName string) error {
		names = append(names, tabName)
		return nil
	}))
	return &names
}

// TestReconcilePanes_FollowsRenamedTabByID is the #1905 fix: renaming a tab must
// keep any open pane bound to it — the pane follows the STABLE ID across the name
// change, it does NOT close. Before the id-keyed reconcile, the changed name read
// as "tab vanished" and closed the pane.
func TestReconcilePanes_FollowsRenamedTabByID(t *testing.T) {
	h, alpha := multiTabHome(t)
	pane := openTestPane(t, h, alpha, 2) // shell-2, id "id-shell2"
	require.Equal(t, 2, pane.Tab())
	require.Same(t, pane, h.store.FindOpenPane(alpha, 2))

	oldKeys := paneTabKeys(alpha)

	// Rename shell-2 -> editor IN PLACE: same id, new name (exactly what
	// Instance.RenameTab / the daemon roster publish, #1904).
	alpha.GetTabs()[2].Name = "editor"

	require.False(t, h.reconcilePanesForTabs(alpha, oldKeys, sameSessionTabs),
		"a pure rename moves no pane slot, so nothing rebinds or closes")
	require.Same(t, pane, h.store.FindOpenPane(alpha, 2),
		"the pane must stay open on the SAME tab (id unchanged), now labelled editor (#1905)")
	require.Equal(t, "editor", alpha.GetTabs()[pane.Tab()].Name)
}

// TestReconcilePanes_ClosesOnCloseRecreateSameName is the #1886 fix: an
// out-of-band close+recreate that reuses the freed name mints a NEW id, so the
// open pane's own tab is truly gone — it must CLOSE, never silently rebind onto
// the imposter tab that inherited the name.
func TestReconcilePanes_ClosesOnCloseRecreateSameName(t *testing.T) {
	h, alpha := multiTabHome(t)
	pane := openTestPane(t, h, alpha, 2) // shell-2, id "id-shell2"
	require.Equal(t, 2, pane.Tab())
	require.Equal(t, 1, h.store.NumOpenPanes())

	oldKeys := paneTabKeys(alpha)

	// Close+recreate shell-2 out-of-band: the slot now holds a DIFFERENT tab that
	// reused the freed name but carries a fresh id (uniqueTabName hands the lowest
	// free name straight back — session/tab_names.go).
	alpha.GetTabs()[2].ID = "id-imposter"

	require.True(t, h.reconcilePanesForTabs(alpha, oldKeys, sameSessionTabs),
		"the pane's own tab id is gone, so the reconcile must act")
	require.Equal(t, 0, h.store.NumOpenPanes(),
		"the pane must CLOSE — never hijack onto the same-named imposter tab (#1886)")
	require.Nil(t, h.store.FindOpenPane(alpha, 2))
}

// TestReconcilePanes_ReorderFollowsByID hardens the reorder path (#1738): a tab
// that changed slots without changing name must still be followed by id, so the
// pane keeps showing its own tab rather than a shifted neighbor.
func TestReconcilePanes_ReorderFollowsByID(t *testing.T) {
	h, alpha := multiTabHome(t)
	pane := openTestPane(t, h, alpha, 1) // shell, id "id-shell"
	require.Equal(t, 1, pane.Tab())

	oldKeys := paneTabKeys(alpha)

	// Swap shell (slot 1) and shell-2 (slot 2) by moving the pointers — a reorder
	// that leaves both names present but relocates each id.
	tabs := alpha.GetTabs()
	tabs[1].ID, tabs[1].Name, tabs[2].ID, tabs[2].Name =
		tabs[2].ID, tabs[2].Name, tabs[1].ID, tabs[1].Name

	require.True(t, h.reconcilePanesForTabs(alpha, oldKeys, sameSessionTabs), "the pane's tab moved slots")
	require.Same(t, pane, h.store.FindOpenPane(alpha, 2),
		"the pane follows its own id (shell) to its new slot 2, not the neighbor now at slot 1")
	require.Equal(t, "shell", alpha.GetTabs()[pane.Tab()].Name)
}

// TestEnterTabA_Esc_EnterTabB_RoutesAndRenders drives the play-test's "enter a
// tab, esc, enter another tab" sequence on ONE multi-tab instance: a pane bound
// to tab A takes input while interactive, Ctrl-] leaves interactive, the pane
// jumps to tab B, and entering B routes input into B's OWN live attachment and
// renders it — never leaking to A's stale attachment. Bind-at-activation (#1820)
// keyed on the stable tab id must compose with the id-keyed pane layer.
func TestEnterTabA_Esc_EnterTabB_RoutesAndRenders(t *testing.T) {
	h := newTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	inst := startedLocalInstance(t, "cycle") // agent (tab 0) + shell (tab 1)
	require.Equal(t, 2, inst.TabCount())
	selectInstance(h, inst)
	resizeHome(h, 120, 40)
	_, _ = stubLiveTermFactory(t)

	// Enter tab A (the agent pane) and type into it.
	openTestPane(t, h, inst, 0)
	paneA := h.focusedOpenPane()
	require.NotNil(t, paneA)
	require.Equal(t, 0, paneA.Tab(), "tab A is the agent tab")
	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)
	require.True(t, h.interactive)
	aFake := focusedFake(h)
	require.NotNil(t, aFake)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")})
	require.Contains(t, aFake.keys, "A", "tab A must receive input while it is the entered tab")

	// Esc out of interactive (Ctrl-] is the host escape hatch).
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	require.False(t, h.interactive, "Ctrl-] returns to nav mode")

	// Jump the SAME pane to tab B (shell), then enter it.
	_, _ = h.handleTabJump(2)
	paneB := h.focusedOpenPane()
	require.Same(t, paneA, paneB, "the same pane jumps tabs")
	require.Equal(t, 1, paneB.Tab(), "the pane now shows tab B (shell)")
	_, cmd = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	runHermeticCmd(t, h, cmd, 0)
	require.True(t, h.interactive, "entering tab B re-enters interactive mode")

	bFake := focusedFake(h)
	require.NotNil(t, bFake)
	require.NotSame(t, aFake, bFake, "tab B binds its OWN live attachment (its own tab id), not A's")

	// Input now routes to B and B renders it.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	assert.Contains(t, bFake.keys, "B", "input must route into tab B after entering it")
	assert.NotContains(t, aFake.keys, "B", "tab A's stale attachment must receive nothing")
	w := h.paneWindows[paneB.ID()]
	require.NotNil(t, w)
	assert.True(t, w.HasLive(), "the pane renders tab B through its live attachment")
	assert.Contains(t, bFake.Render(80, 24, false), "B", "tab B's grid renders the typed input")
}

// ----------------------------------------------------------------------------
// Codex review findings on #1906 — the reconcile edges of the same bug classes.
// ----------------------------------------------------------------------------

// TestCloseTab_PaneFocusedClosePreservesTreeTab is codex finding 1: a
// pane-focused close removes a tab the TREE is not on, so the tree must keep
// pointing at its OWN tab. Closing tab 4 while the tree sits on tab 2 used to
// slam the tree's active tab to idx-1 (tab 3), so every later tree-driven
// preview/open targeted the wrong tab.
func TestCloseTab_PaneFocusedClosePreservesTreeTab(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(1) // tree on shell (slot 1)
	h.menu.SetActiveTab(1)

	pane := openTestPane(t, h, alpha, 1)
	_, _ = h.handleTabJump(4) // pane jumps to shell-3 (slot 3); tree stays on slot 1
	require.Equal(t, 3, pane.Tab())
	require.Equal(t, 1, h.store.ActiveTab())

	recordCloseTab(t, h)
	_, _ = h.handleCloseTab()

	require.Equal(t, 3, alpha.TabCount(), "the pane's tab (shell-3) was closed")
	assert.Equal(t, 1, h.store.ActiveTab(),
		"the tree was on shell (slot 1) and shell did not move — its active tab must be untouched")
	assert.Equal(t, "shell", alpha.GetTabs()[h.store.ActiveTab()].Name,
		"the tree still points at its OWN tab, resolved by id across the close")
}

// TestCloseTab_ClosingBelowTreeTabShiftsIt is the other half of finding 1: when
// the closed tab sits BELOW the tree's active tab, every higher slot shifts down
// and the tree's tab must follow the shift — still landing on the same tab.
func TestCloseTab_ClosingBelowTreeTabShiftsIt(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(3) // tree on shell-3 (slot 3)
	h.menu.SetActiveTab(3)

	pane := openTestPane(t, h, alpha, 3)
	_, _ = h.handleTabJump(2) // pane jumps DOWN to shell (slot 1)
	require.Equal(t, 1, pane.Tab())
	require.Equal(t, 3, h.store.ActiveTab())

	recordCloseTab(t, h)
	_, _ = h.handleCloseTab() // closes shell (slot 1), below the tree's tab

	assert.Equal(t, 2, h.store.ActiveTab(), "the tree's tab shifted down one slot with the close")
	assert.Equal(t, "shell-3", alpha.GetTabs()[h.store.ActiveTab()].Name,
		"the tree still shows the SAME tab it was on, at its new slot")
}

// TestCloseTab_ClosingTheTreesOwnTabFallsBackToNeighbor pins the original #930
// behavior: when the tab that dies IS the tree's active tab, the tree lands on
// the left neighbour.
func TestCloseTab_ClosingTheTreesOwnTabFallsBackToNeighbor(t *testing.T) {
	h, _ := multiTabHome(t)
	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(2)
	h.menu.SetActiveTab(2)

	recordCloseTab(t, h)
	_, _ = h.handleCloseTab()

	assert.Equal(t, 1, h.store.ActiveTab(), "closing the tree's own tab lands on the left neighbour")
}

// TestPaneJump_PinSurvivesUnseededEpoch is codex finding 7: on a restored /
// startup pane the user can press a number BEFORE any selectionChanged has run,
// so lastSelectionKey is empty. Pinning against that unseeded epoch pinned 0, and
// the jump's own trailing selectionChanged then read the unchanged cursor as a
// move and bumped to 1 — staling the pin before the guard checked it and letting
// the #1885 repaint back in. The jump must seed the epoch before pinning.
func TestPaneJump_PinSurvivesUnseededEpoch(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(1)
	h.menu.SetActiveTab(1)
	pane := openTestPane(t, h, alpha, 1)

	// Simulate the startup / restored-pane window: no selectionChanged has primed
	// the epoch yet.
	h.lastSelectionKey = ""
	h.selectionEpoch = 0
	h.paneJumpIntent = map[int]uint64{}

	_, _ = h.handleTabJump(4)

	require.Equal(t, 3, pane.Tab(), "the jump commits")
	assert.True(t, h.paneJumpIntentPinned(pane.ID()),
		"the pin must still be current after the jump's own selectionChanged (seed before pinning)")
	assert.Nil(t, h.panePreviewTxn,
		"an unseeded epoch must not let the tree-cursor preview repaint the committed jump (#1885)")
}

// TestPaneJump_TreeTabChangeStalesThePin is codex finding 3: after a pane-focused
// jump pins intent, the user returns focus to the tree and presses a number to
// change the selected row's active tab. SyncCursorToActiveTab leaves the cursor
// on the instance row, so the selection key looked unchanged unless it includes
// store.ActiveTab() — and the stale pin then cancelled the very preview the user
// asked for (the invariant-eats-the-gesture class).
func TestPaneJump_TreeTabChangeStalesThePin(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(1)
	h.menu.SetActiveTab(1)
	pane := openTestPane(t, h, alpha, 1)

	_, _ = h.handleTabJump(4) // pane-focused: pins intent
	require.True(t, h.paneJumpIntentPinned(pane.ID()))

	// Back to the tree; a tree-focus number key retargets the row's ACTIVE TAB
	// without necessarily moving the cursor row.
	h.focusRegion(layout.RegionTree)
	_, _ = h.handleTabJump(3)

	require.Equal(t, 2, h.store.ActiveTab(), "the tree-focus jump retargets the active tab")
	assert.False(t, h.paneJumpIntentPinned(pane.ID()),
		"an active-tab change is a selection move: it must bump the epoch and stale the pin")
}

// TestSwapSameTitle_PreservesOpenPanes is codex finding 4 — a regression this
// PR's id-keying introduced. A same-title kill/recreate swaps in an ENTIRELY NEW
// session whose tabs carry freshly minted ids, so comparing the corpse's ids
// against the replacement's reads every tab as missing and closes every pane the
// swap exists to preserve. The swap is the REPLACED-session domain: panes follow
// the equivalent slot by name (agent→agent), as they did before #1906.
func TestSwapSameTitle_PreservesOpenPanes(t *testing.T) {
	h := newTestHome(t)
	stale := instanceWithFakeBackend(t, "dup")
	stale.AddTabForTest("agent", session.TabKindAgent)
	stale.AddTabForTest("shell", session.TabKindShell)
	for i, tab := range stale.GetTabs() {
		tab.ID = []string{"stale-agent", "stale-shell"}[i]
	}
	h.store.AddInstance(stale)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)

	pane := openTestPane(t, h, stale, 1) // a pane on the corpse's shell tab
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The recreated session: same title, same tab NAMES, brand-new ids.
	recreated := instanceWithFakeBackend(t, "dup")
	recreated.AddTabForTest("agent", session.TabKindAgent)
	recreated.AddTabForTest("shell", session.TabKindShell)
	for i, tab := range recreated.GetTabs() {
		tab.ID = []string{"fresh-agent", "fresh-shell"}[i]
	}
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	require.True(t, h.swapInstanceFromSnapshot(recreated.ToInstanceData()))

	require.Equal(t, 1, h.store.NumOpenPanes(),
		"a same-title swap must PRESERVE the open pane — the replacement's ids differ by construction")
	assert.Same(t, pane, h.store.FindOpenPane(recreated, 1),
		"the pane follows the equivalent slot (shell) on the replacement session")
	assert.Same(t, recreated, pane.Instance(), "and is re-pointed at the live row")
}

// TestSnapshot_ClosesPaneWhenTabIDChanges is codex finding 2, driven through the
// REAL production path (updateInstanceFromSnapshot), not the reconcile helper in
// isolation. An out-of-band close+recreate reusing the freed name gives the roster
// row a NEW id. The name-keyed reconcile called that "unchanged", so the pane
// reconcile — gated on tabsChanged — never ran and the pane stayed bound to a dead
// id, silently showing a different process (#1886).
func TestSnapshot_ClosesPaneWhenTabIDChanges(t *testing.T) {
	h := newTestHome(t)
	// A genuinely STARTED instance (worktree + mock tmux): ReconcileTabsFromData
	// no-ops on a fake-backend row, so only this harness reaches the real path.
	inst := startedLocalInstance(t, "oob") // agent + shell, real minted ids
	require.Equal(t, 2, inst.TabCount())
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	pane := openTestPane(t, h, inst, 1) // pane on shell
	require.Equal(t, 1, h.store.NumOpenPanes())
	require.Same(t, pane, h.store.FindOpenPane(inst, 1))
	oldShellID := inst.GetTabs()[1].ID
	require.NotEmpty(t, oldShellID)

	// The daemon's next snapshot: "shell" is back, but it is a DIFFERENT tab —
	// another client closed the old one and created a new one that reused the name.
	data := inst.ToInstanceData()
	for i := range data.Tabs {
		if data.Tabs[i].ID == oldShellID {
			data.Tabs[i].ID = "id-shell-new"
		}
	}
	h.updateInstanceFromSnapshot(inst, data)

	require.Equal(t, "id-shell-new", inst.GetTabs()[1].ID,
		"the roster now carries the recreated tab's new id")
	assert.Equal(t, 0, h.store.NumOpenPanes(),
		"the pane's tab id left the roster: it must CLOSE, never keep rendering the imposter tab (#1886)")
}

// TestSnapshot_AgentIDHealKeepsPaneOpen is the pane-layer half of the agent-tab
// self-heal, driven through the real production path. The agent tab is a
// positional singleton whose id is mutable addressing, not identity: the snapshot
// CORRECTS a locally-minted id that diverged from the daemon's. Keyed by id like
// every other slot, that heal would read as "the tab vanished" and close the agent
// pane — trading a blank pane for a disappearing one. paneTabKeys keys the agent
// slot by name so the pane rides through, and liveBindCandidate (which keys the
// live stream ON the id) re-dials it onto the working id.
func TestSnapshot_AgentIDHealKeepsPaneOpen(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "heal")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	pane := openTestPane(t, h, inst, 0) // pane on the AGENT tab
	require.Equal(t, 1, h.store.NumOpenPanes())
	staleAgentID := inst.GetTabs()[0].ID
	require.NotEmpty(t, staleAgentID, "precondition: the agent id is locally minted")

	// The daemon's roster carries a different id for the agent row — the ordinary
	// legacy/restart divergence, not an exotic one.
	data := inst.ToInstanceData()
	data.Tabs[0].ID = "daemon-agent-id"
	h.updateInstanceFromSnapshot(inst, data)

	healed, ok := inst.TabIDAt(0)
	require.True(t, ok)
	require.Equal(t, "daemon-agent-id", healed, "precondition: the agent tab healed its id")
	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the agent tab did not go anywhere — correcting its id must NOT close its pane")
	assert.Same(t, pane, h.store.FindOpenPane(inst, 0),
		"the agent pane stays bound to slot 0 across the heal")
}

// TestCloseTab_IDLessTreeTabTracksByOrdinal covers the id-less window. A tab the
// user just created is ID-LESS by design (AttachShellTab leaves Tab.ID empty
// until the next snapshot backfills the daemon's), so the tree's tab cannot be
// found by id at all. With the id lookup skipped, `next` fell back to idx-1 —
// where idx is the FOCUSED PANE's closed tab, not the tree's — so a pane-focused
// close of a DIFFERENT tab jumped the tree to a neighbour of the wrong tab, even
// though the tree's own tab survived and merely shifted. The ordinal is the only
// key available in that window, so every id lookup needs an ordinal fallback.
func TestCloseTab_IDLessTreeTabTracksByOrdinal(t *testing.T) {
	h, alpha := multiTabHome(t)

	// The freshly-created tabs are id-less, exactly as AttachShellTab leaves them
	// before the daemon's snapshot backfills their ids.
	for _, tab := range alpha.GetTabs() {
		tab.ID = ""
	}

	// The tree sits on shell-3 (slot 3) — the newest tab.
	h.store.SetActiveTab(3)
	h.menu.SetActiveTab(3)

	// The focused pane is jumped BACK to an older tab, shell (slot 1), and closes it.
	pane := openTestPane(t, h, alpha, 3)
	_, _ = h.handleTabJump(2)
	require.Equal(t, 1, pane.Tab(), "precondition: the focused pane shows the older tab (slot 1)")
	require.Equal(t, 3, h.store.ActiveTab(), "precondition: the tree is still on shell-3 (slot 3)")

	recordCloseTab(t, h)
	_, _ = h.handleCloseTab()

	// shell (slot 1) died, so the tree's shell-3 shifted 3 → 2. It must FOLLOW its
	// own tab, not land on a neighbour of the pane's closed tab.
	require.Equal(t, 3, alpha.TabCount(), "precondition: the pane's tab was the one closed")
	assert.Equal(t, "shell-3", alpha.GetTabs()[h.store.ActiveTab()].Name,
		"the tree's own tab survived the pane-focused close — it must still be selected, merely shifted")
	assert.Equal(t, 2, h.store.ActiveTab(),
		"shell-3 shifted 3 → 2 when the lower tab closed; the tree must track it by ordinal in the id-less window")
}

// TestSwapSameTitle_StalesPinnedPaneJump is the selection-epoch half of the
// same-title trap. Titles are not identity: a kill/recreate swaps in an ENTIRELY
// NEW session object while the cursor row and the title stay put, so a
// title-only selection key reads the swap as "nothing moved" and never advances
// the epoch. A pane jump pinned against the CORPSE then stays live over the
// replacement, cancelling its previews until the user happens to navigate away.
// The stable session id makes the swap the logical selection change it is.
func TestSwapSameTitle_StalesPinnedPaneJump(t *testing.T) {
	h := newTestHome(t)
	stale := instanceWithFakeBackend(t, "dup")
	stale.AddTabForTest("agent", session.TabKindAgent)
	stale.AddTabForTest("shell", session.TabKindShell)
	for i, tab := range stale.GetTabs() {
		tab.ID = []string{"stale-agent", "stale-shell"}[i]
	}
	h.store.AddInstance(stale)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)

	// A pinned pane jump on the corpse — the state the stale pin lives in.
	pane := openTestPane(t, h, stale, 0)
	_, _ = h.handleTabJump(2)
	require.Equal(t, 1, pane.Tab(), "precondition: the pane jumped to the shell tab")
	require.True(t, h.paneJumpIntentPinned(pane.ID()), "precondition: the jump is pinned")
	epochBefore := h.selectionEpoch

	// The recreated session: same title, same tab names, a DIFFERENT stable id —
	// and the sidebar cursor never moves.
	recreated := instanceWithFakeBackend(t, "dup")
	recreated.AddTabForTest("agent", session.TabKindAgent)
	recreated.AddTabForTest("shell", session.TabKindShell)
	for i, tab := range recreated.GetTabs() {
		tab.ID = []string{"fresh-agent", "fresh-shell"}[i]
	}
	require.NotEqual(t, stale.ID, recreated.ID, "precondition: the replacement is a different session")
	require.Equal(t, stale.Title, recreated.Title, "precondition: under an unchanged title")
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))
	require.True(t, h.swapInstanceFromSnapshot(recreated.ToInstanceData()))

	// The swap is a logical selection change: the next selectionChanged must see it.
	_ = h.selectionChanged()
	assert.Greater(t, h.selectionEpoch, epochBefore,
		"a same-title swap replaced the selected session — the epoch must advance like any other selection change")
	assert.False(t, h.paneJumpIntentPinned(pane.ID()),
		"the pin was made against the corpse: it must not keep cancelling previews for the replacement session")
}
