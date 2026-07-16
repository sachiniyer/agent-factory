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

	require.False(t, h.reconcilePanesForTabs(alpha, oldKeys),
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

	require.True(t, h.reconcilePanesForTabs(alpha, oldKeys),
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

	require.True(t, h.reconcilePanesForTabs(alpha, oldKeys), "the pane's tab moved slots")
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
