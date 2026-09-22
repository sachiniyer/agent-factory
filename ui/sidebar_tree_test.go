package ui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// newTreeSidebar builds a sidebar over a fresh projection with n instances
// titled t-00..t-NN, each carrying a real agent + shell tab pair (the shape of
// a started instance after `t`) so the tree shows two tab slots per instance.
// Since #1100 the slot list mirrors the real tabs — there is no padding.
func newTreeSidebar(t *testing.T, n int) *Sidebar {
	t.Helper()
	s := NewSidebar(store.NewProjection())
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: fmt.Sprintf("t-%02d", i), Path: dir, Program: "test",
		})
		require.NoError(t, err)
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	return s
}

// tabRowCount counts the tab child rows in the flattened list, after the same
// lazy store sync every public read performs.
func tabRowCount(s *Sidebar) int {
	s.syncFromStore()
	n := 0
	for _, item := range s.visibleItems {
		if item.IsTab {
			n++
		}
	}
	return n
}

// TestSidebarTreeRendersTabChildren pins the first visible change of #1024
// PR 3: the selected instance's tabs render as indented child rows with the
// same labels (and 1-based numbers) as the tab bar, a tab bound to an open pane
// carries the " · open" marker, and non-selected instances stay collapsed.
func TestSidebarTreeRendersTabChildren(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSize(40, 24)

	// Nothing selected yet: no tab rows, both instances collapsed.
	out := s.String()
	assert.NotContains(t, out, "├", "no tab children before a selection exists")

	s.SetSelectedInstance(0)
	inst := s.proj.GetInstances()[0]
	pane := s.proj.AddOpenPane(inst, 0)
	out = s.String()
	assert.Contains(t, out, "├ 1 Agent · open", "agent tab child with slot number and active marker")
	assert.Contains(t, out, "└ 2 › Terminal", "terminal tab child with └ terminator")
	assert.Contains(t, out, "▾", "selected instance shows the expanded arrow")
	assert.Contains(t, out, "▸", "non-selected instance stays collapsed")
	assert.NotRegexp(t, regexp.MustCompile(`\b\d+\.\s+t-\d\d`), out,
		"instance rows must not render their position number")
	assert.Equal(t, 2, tabRowCount(s), "only the selected instance contributes tab rows")

	// The marker follows the open pane's tab binding.
	require.True(t, s.proj.RebindOpenPane(pane, inst, 1))
	out = s.String()
	assert.Contains(t, out, "└ 2 › Terminal · open")
	assert.NotContains(t, out, "├ 1 Agent · open")
}

// TestSidebarTreeOpenMarkerClearsWithLastPane is the first #3996 regression:
// hiding the only workspace pane leaves no tab marked open in the rail.
func TestSidebarTreeOpenMarkerClearsWithLastPane(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)
	inst := s.proj.GetInstances()[0]
	pane := s.proj.AddOpenPane(inst, 0)
	require.Contains(t, s.String(), "├ 1 Agent · open")

	require.True(t, s.proj.CloseOpenPane(pane))
	assert.NotContains(t, s.String(), "· open",
		"the rail must render no open marker when no workspace pane is open")
}

// TestSidebarTreeOpenMarkerFollowsPaneNotCursor is the second #3996
// regression: moving the rail cursor previews another tab without moving the
// marker away from the tab that remains bound to the workspace pane.
func TestSidebarTreeOpenMarkerFollowsPaneNotCursor(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)
	inst := s.proj.GetInstances()[0]
	s.proj.AddOpenPane(inst, 0)

	s.Down() // Agent tab row.
	s.Down() // Terminal tab row; the Agent pane remains open.
	require.Equal(t, 1, s.proj.ActiveTab())

	out := s.String()
	assert.Contains(t, out, "├ 1 Agent · open",
		"the marker must follow the tab shown in the workspace pane")
	assert.NotContains(t, out, "└ 2 › Terminal · open",
		"the rail cursor's preview tab must not inherit the open marker")
}

// TestSidebarTreeFreshInstanceSingleTabRow pins the #1100 tree rendering: a
// fresh instance holds only its agent tab, so its expanded subtree is exactly
// one child row — no phantom "Terminal" row for a tab that doesn't exist —
// and the on-demand shell tab (`t`) grows it to two.
func TestSidebarTreeFreshInstanceSingleTabRow(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fresh", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.AddTabForTest("agent", session.TabKindAgent)
	addTestInstance(s, inst)
	s.proj.AddOpenPane(inst, 0)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)

	require.Equal(t, 1, tabRowCount(s), "fresh instance: exactly one tab row")
	out := s.String()
	assert.Contains(t, out, "└ 1 Agent · open", "the agent tab is the only — and last — child row")
	assert.NotContains(t, out, "Terminal", "no phantom Terminal row before t is pressed")

	// `t` materializes the shell tab; the tree grows a real second row.
	inst.AddTabForTest("shell", session.TabKindShell)
	assert.Equal(t, 2, tabRowCount(s), "after t: the on-demand terminal is the second row")
	assert.Contains(t, s.String(), "└ 2 › Terminal")
}

// TestSidebarTreeSelectionMoveCollapsesPrevious pins the collapse-by-default
// rule: moving the selection to another instance folds the previous one, so
// the row count stays ≈ instances + selected-instance tabs.
func TestSidebarTreeSelectionMoveCollapsesPrevious(t *testing.T) {
	s := newTreeSidebar(t, 3)

	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s))

	s.SetSelectedInstance(2)
	assert.Equal(t, 2, tabRowCount(s), "previous instance folded; new one expanded")
	sel := s.GetSelection()
	assert.Equal(t, 2, sel.ItemIndex)
	assert.False(t, sel.IsTab)
}

// TestSidebarTreeExplicitCollapseExpand pins the h/← and l/→ tree verbs: a tab
// row collapses to its parent, an expanded instance folds in place, l re-opens
// it, and moving the selection away then back clears the explicit collapse
// (auto-expand applies again).
func TestSidebarTreeExplicitCollapseExpand(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSelectedInstance(0)

	// Down onto the first tab row, then collapse: cursor lands on the parent
	// instance row and the children fold.
	s.Down()
	require.True(t, s.GetSelection().IsTab)
	s.CollapseSection()
	sel := s.GetSelection()
	assert.False(t, sel.IsTab, "collapse from a tab row folds to the parent instance row")
	assert.Equal(t, 0, sel.ItemIndex)
	assert.Equal(t, 0, tabRowCount(s))

	// l/→ re-expands in place.
	s.ExpandSection()
	assert.Equal(t, 2, tabRowCount(s))

	// Collapse again, move the selection away and back: the explicit collapse
	// is cleared, so the re-selected instance auto-expands.
	s.CollapseSection()
	require.Equal(t, 0, tabRowCount(s))
	s.SetSelectedInstance(1)
	s.SetSelectedInstance(0)
	assert.Equal(t, 2, tabRowCount(s), "re-selecting auto-expands; explicit collapse does not persist")
}

// TestSidebarTreeCollapseSurvivesDownFromInstanceRow pins the
// c4757da9 regression: a single Down off an explicitly-collapsed (h/←)
// instance row must NOT clear treeCollapsed and re-expand the SAME instance
// into its own tab 0. The field's doc states it is "Cleared when the selection
// moves to a different instance"; a Down that targets the same instance's
// tabs is not a cross-instance move, so the fold must persist and the cursor
// must stay on the instance row (not dive into hidden tabs). Pre-fix the
// unconditional s.treeCollapsed = "" in selectTabStop reverted the collapse.
func TestSidebarTreeCollapseSurvivesDownFromInstanceRow(t *testing.T) {
	s := newTreeSidebar(t, 3) // instances t-00, t-01, t-02
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "instance 0 auto-expanded")

	// h/← from instance 0's own row folds its tab children in place.
	s.CollapseSection()
	require.Equal(t, 0, tabRowCount(s))
	require.Equal(t, "t-00", s.treeCollapsed)
	sel := s.GetSelection()
	require.False(t, sel.IsTab, "collapse leaves the cursor on the instance row")

	// A single Down press must not undo the fold. The pre-fix code
	// unconditionally cleared treeCollapsed in selectTabStop and dove into
	// tab 0 of the SAME instance.
	s.Down()
	assert.Equal(t, 0, tabRowCount(s), "folded tabs stay hidden after Down")
	assert.False(t, s.GetSelection().IsTab, "cursor does not dive into folded tabs")
	assert.Equal(t, "t-00", s.treeCollapsed, "explicit collapse survives same-instance Down")
	// The cursor stays on instance 0's row.
	sel = s.GetSelection()
	assert.Equal(t, 0, sel.ItemIndex)
	assert.False(t, sel.IsTab)
}

// TestSidebarTreeCollapseDownSameInstanceKeepsActiveTab pins the second half
// of the regression the inline review flagged: when an explicitly collapsed
// instance carries a NONZERO active tab, a Down whose target is the same
// instance's tabs cannot land (the tab rows are folded away), so it must be a
// consumed no-op BEFORE SetActiveTab — otherwise selectTabStop resets the
// active tab to 0, silently retargeting the preview pane while the fold
// visually persists. The sibling test above uses the default active tab (0),
// which a spurious SetActiveTab(0) cannot distinguish from "unchanged"; this
// one uses tab 1 so the reset is observable.
func TestSidebarTreeCollapseDownSameInstanceKeepsActiveTab(t *testing.T) {
	s := newTreeSidebar(t, 3) // instances t-00, t-01, t-02
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "instance 0 auto-expanded")

	// Drive the active tab to the (nonzero) terminal tab, then fold instance 0
	// in place from its row.
	s.proj.SetActiveTab(1)
	require.Equal(t, 1, s.proj.ActiveTab(), "active tab is the terminal tab before collapse")
	s.CollapseSection()
	require.Equal(t, "t-00", s.treeCollapsed)
	require.Equal(t, 0, tabRowCount(s))
	require.False(t, s.GetSelection().IsTab, "collapse leaves the cursor on the instance row")

	// A same-instance Down is a consumed no-op: the fold and cursor survive…
	s.Down()
	assert.Equal(t, 0, tabRowCount(s), "folded tabs stay hidden after Down")
	assert.False(t, s.GetSelection().IsTab, "cursor does not dive into folded tabs")
	assert.Equal(t, "t-00", s.treeCollapsed, "explicit collapse survives same-instance Down")
	sel := s.GetSelection()
	assert.Equal(t, 0, sel.ItemIndex)
	assert.False(t, sel.IsTab)
	// …and the store's active tab is NOT reset to 0.
	assert.Equal(t, 1, s.proj.ActiveTab(), "active tab preserved when same-instance Down is a no-op")
}

// TestSidebarTreeCollapseClearsOnCrossInstanceSelect pins that the fix did
// not weaken the documented cross-instance clear: moving the selection to a
// different instance still clears treeCollapsed so every newly selected
// instance starts auto-expanded (collapse-by-default applies to non-selected
// instances).
func TestSidebarTreeCollapseClearsOnCrossInstanceSelect(t *testing.T) {
	s := newTreeSidebar(t, 3)
	s.SetSelectedInstance(0)
	s.CollapseSection()
	require.Equal(t, "t-00", s.treeCollapsed)
	require.Equal(t, 0, tabRowCount(s))

	s.SetSelectedInstance(1) // different instance
	assert.Equal(t, 1, s.GetSelection().ItemIndex)
	assert.Equal(t, "", s.treeCollapsed, "cleared — a different instance is selected")
	assert.Equal(t, 2, tabRowCount(s), "new instance auto-expands")
}

// TestSidebarTreeCollapseDownFromHeaderSelectsTab pins the second inline Codex
// finding's regression (review 5274299199): the same-instance-collapsed no-op
// must fire ONLY when the cursor rests on the instance row. A second h/← from
// the collapsed-instance row jumps to the Instances header and folds the
// section, but pushSelection skips header rows, so the store's sticky
// selection and treeCollapsed both still name the just-collapsed instance
// while the cursor is on the header. Keying the no-op off the store pinned
// every subsequent Down on the header — the first stop's tab belonged to the
// still-sticky instance, the guard consumed the move, and neither the section
// nor the instance re-expanded. Navigation from a header must instead clear
// the override and select the target tab normally.
func TestSidebarTreeCollapseDownFromHeaderSelectsTab(t *testing.T) {
	s := newTreeSidebar(t, 3) // instances t-00, t-01, t-02
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "instance 0 auto-expanded")

	// h/← from instance 0's row folds its tab children in place.
	s.CollapseSection()
	require.Equal(t, 0, tabRowCount(s))
	require.Equal(t, "t-00", s.treeCollapsed)
	require.False(t, s.GetSelection().IsTab, "cursor on the instance row")

	// A second h/← jumps to the Instances header and folds the section; the
	// store's sticky selection and treeCollapsed both still name t-00.
	s.CollapseSection()
	require.True(t, s.GetSelection().IsHeader, "cursor moved to the Instances header")
	require.Equal(t, "t-00", s.treeCollapsed, "sticky collapse survives the header jump")

	// Down must NOT be consumed: it clears the override, expands the section
	// and lands on the first tab of the formerly sticky instance.
	s.Down()
	sel := s.GetSelection()
	assert.True(t, sel.IsTab, "Down from the header selects a tab row, not a no-op")
	assert.Equal(t, 0, sel.ItemIndex, "lands on instance 0's first tab")
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, "", s.treeCollapsed, "the same-instance override is cleared")
	assert.Equal(t, 2, tabRowCount(s), "instance 0 re-expanded so the tab row is visible")
}

// TestSidebarTreeTabCursorDrivesActiveTab pins the selection tab dimension:
// landing the cursor on a tab row sets the store's active tab (which is what
// retargets the content pane), and GetSelectedInstance still resolves the
// parent instance from a tab row.
func TestSidebarTreeTabCursorDrivesActiveTab(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSelectedInstance(0)
	require.Equal(t, 0, s.proj.ActiveTab())

	s.Down() // tab 0
	assert.Equal(t, 0, s.proj.ActiveTab())
	s.Down() // tab 1
	assert.Equal(t, 1, s.proj.ActiveTab())
	require.NotNil(t, s.GetSelectedInstance())
	assert.Equal(t, "t-00", s.GetSelectedInstance().Title,
		"a tab row still selects its parent instance")

	s.Up()
	assert.Equal(t, 0, s.proj.ActiveTab(), "moving back up re-selects tab 0")
}

// TestSidebarTreeSyncCursorToActiveTab pins the 1-9/tab-cycle follow rule: the
// cursor follows an active-tab change only when it already rests on a tab row;
// on the instance row the pre-tree behavior is preserved (cursor stays put).
func TestSidebarTreeSyncCursorToActiveTab(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSelectedInstance(0)

	// Cursor on the instance row: a jump must not move it.
	s.proj.SetActiveTab(1)
	s.SyncCursorToActiveTab()
	assert.False(t, s.GetSelection().IsTab, "cursor on the instance row stays put")

	// Cursor on a tab row: it follows the jump.
	s.Down() // tab 0 (also resets active tab to 0)
	require.True(t, s.GetSelection().IsTab)
	require.Equal(t, 0, s.proj.ActiveTab())
	s.proj.SetActiveTab(1)
	s.SyncCursorToActiveTab()
	sel := s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor followed the tab jump")
}

// TestSidebarTreeSyncCursorSurvivesStructureRebuild pins the PR #1081
// play-test fix at the sidebar level: an active-tab change made TOGETHER with
// a tab-slot change (what t/w do) must survive SyncCursorToActiveTab. The
// slot change trips the structure rebuild inside syncFromStore, whose
// pushSelection re-asserts the cursor row's old tab index — the method must
// capture and re-apply the intended target rather than read it post-sync.
func TestSidebarTreeSyncCursorSurvivesStructureRebuild(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSelectedInstance(0)
	s.Down() // tab row 0
	s.Down() // tab row 1
	require.Equal(t, 1, s.proj.ActiveTab())

	// Simulate a new tab: the instance grows a third slot in place (no
	// store version bump) and the handler selects the fresh tab.
	inst := s.proj.GetInstances()[0]
	inst.AddTabForTest("proc", session.TabKindProcess)
	s.proj.SetActiveTab(2)
	s.SyncCursorToActiveTab()

	assert.Equal(t, 2, s.proj.ActiveTab(),
		"the intended active tab must survive the structure rebuild")
	sel := s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 2, sel.TabIndex, "cursor must land on the intended tab row")

	// And the shrink direction (what w does): drop back to slot 1.
	require.NoError(t, inst.DropClosedTab(2))
	s.proj.SetActiveTab(1)
	s.SyncCursorToActiveTab()
	assert.Equal(t, 1, s.proj.ActiveTab())
	sel = s.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
}

// TestSidebarTreeCloseLastTabStaysOnInstance is the #1084 regression: with the
// cursor on the LAST tab row of the selected instance and ANOTHER instance
// below it, closing that tab (what handleCloseTab does: DropClosedTab +
// SetActiveTab(idx-1) + SyncCursorToActiveTab) must keep the selection within
// the acting instance's subtree — the shrunk row list drops the old tab-row
// flat index onto the trailing instance's row, and the pre-fix code committed
// that drift as the display selection before it could re-pin by title.
func TestSidebarTreeCloseLastTabStaysOnInstance(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSelectedInstance(0)
	s.Down() // tab row 0 of t-00
	s.Down() // tab row 1 of t-00 (the last tab), active tab = 1
	require.True(t, s.GetSelection().IsTab)
	require.Equal(t, 1, s.GetSelection().TabIndex)

	// Simulate handleCloseTab on the last tab: the daemon-authoritative drop
	// removes the slot in place, the handler selects the left neighbor, then
	// re-pins the tree cursor.
	inst := s.proj.GetInstances()[0]
	require.NoError(t, inst.DropClosedTab(1))
	s.proj.SetActiveTab(0)
	s.SyncCursorToActiveTab()

	// Selection must stay on the acting instance (t-00), not drift to t-01.
	require.NotNil(t, s.GetSelectedInstance())
	assert.Equal(t, "t-00", s.GetSelectedInstance().Title,
		"closing the last tab must not drift the selection to the trailing instance")
	sel := s.GetSelection()
	assert.Equal(t, 0, sel.ItemIndex, "cursor stays on t-00's row/subtree")
	assert.True(t, sel.IsTab, "cursor lands on the surviving (agent) tab row")
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, 0, s.proj.ActiveTab(), "the intended active tab survives the rebuild")
	// The acting instance stays expanded; the trailing one stays folded.
	assert.Equal(t, 1, tabRowCount(s), "only t-00's surviving tab row is present")
}

// TestSidebarTreeRepinPreservesTabSelection is the tree extension of the #969
// re-pin: a reconcile that removes a preceding instance re-pins the selection
// by title, and the cursor must return to the SAME TAB ROW it was on, not just
// the instance row.
func TestSidebarTreeRepinPreservesTabSelection(t *testing.T) {
	s := newTreeSidebar(t, 3)
	s.SetSelectedInstance(1)
	s.Down()
	s.Down() // tab row 1 of t-01
	sel := s.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)

	// Reconcile removes the instance ABOVE the selection and re-pins (what
	// reconcileSnapshot does: removal + SelectInstance assertion by title).
	target := s.proj.GetInstanceByTitle("t-01")
	require.True(t, s.proj.RemoveInstanceByTitle("t-00"))
	s.proj.SelectInstance(target)

	sel = s.GetSelection()
	assert.True(t, sel.IsTab, "re-pin must restore the tab sub-selection")
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, s.proj.ActiveTab())
	require.NotNil(t, s.GetSelectedInstance())
	assert.Equal(t, "t-01", s.GetSelectedInstance().Title)
}

// TestSidebarTreeSwapPreservesTabSelection covers the #765 kill+recreate swap
// in the tree world: the selected instance is replaced by a rebuilt same-title
// pointer and re-pinned; expansion and the tab sub-selection must survive.
func TestSidebarTreeSwapPreservesTabSelection(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSelectedInstance(0)
	s.Down() // tab row 0
	require.True(t, s.GetSelection().IsTab)

	rebuilt, err := session.NewInstance(session.InstanceOptions{
		Title: "t-00", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	addAgentShellTabs(rebuilt)
	require.True(t, s.proj.ReplaceInstanceByTitle("t-00", rebuilt))
	s.proj.SelectInstance(rebuilt)

	sel := s.GetSelection()
	assert.True(t, sel.IsTab, "swap keeps the cursor on the tab row")
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, 2, tabRowCount(s), "same-title swap keeps the subtree expanded")
	assert.Same(t, rebuilt, s.GetSelectedInstance())
}

// TestSidebarTreeTransientRowsCollapse pins the tree treatment of transient
// rows: a Deleting (or Loading) instance is never expandable — its tab
// children fold and the ▾ arrow disappears, even while it is the selection.
func TestSidebarTreeTransientRowsCollapse(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s))

	inst := s.proj.GetInstances()[0]
	inst.SetStatusForTest(session.Deleting)
	// Status flips in place (no store version bump) — the structure signature
	// must still pick it up on the next read.
	assert.Equal(t, 0, tabRowCount(s), "deleting instance folds its tab children")
	out := s.String()
	assert.Contains(t, out, "[deleting]")
	assert.NotContains(t, out, "├", "no tab children while deleting")
	assert.NotContains(t, out, "▾", "no expanded arrow while deleting")

	inst.SetStatusForTest(session.Ready)
	assert.Equal(t, 2, tabRowCount(s), "back to Ready re-expands the selection")
}

// TestSidebarTreeOutOfBandTabAppears pins the live-display property (#959) in
// the tree: a tab reconciled onto the SAME instance pointer (no store version
// bump, as the snapshot reconcile does) must appear as a child row on the next
// read.
func TestSidebarTreeOutOfBandTabAppears(t *testing.T) {
	s := newTreeSidebar(t, 1)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s))

	inst := s.proj.GetInstances()[0]
	inst.AddTabForTest("btop", session.TabKindProcess)

	assert.Equal(t, 3, tabRowCount(s), "in-place tab growth must surface without a store bump")
	assert.Contains(t, s.String(), "└ 3 › btop")
}

// TestSidebarTreeWindowingWithTabRows extends the #787 windowing guarantee to
// the tree: with the selection resting on a tab row deep in a long list, the
// sidebar still renders exactly its allocation and the selected tab row is
// inside the window.
func TestSidebarTreeWindowingWithTabRows(t *testing.T) {
	const w, h = 40, 20
	s := newTreeSidebar(t, 25)
	s.SetSize(w, h)

	s.SetSelectedInstance(12)
	s.Down()
	s.Down() // tab row 1 of t-12
	require.True(t, s.GetSelection().IsTab)

	out := s.String()
	require.Equal(t, h, renderedLineCount(out),
		"sidebar must render exactly the allocated height with tab rows present")
	assert.Contains(t, out, "t-12", "selected instance must be inside the window")
	assert.Contains(t, out, "└ 2 › Terminal", "selected tab row must be inside the window")
}

// TestSidebarUltraNarrowNoOverflow pins the #646 no-overflow guarantee for
// EVERY sidebar row kind at ultra-narrow allocations — section headers, the
// title bar, instance rows, tab rows, and the ▲/▼
// window indicators. Greptile/T-Rex reproduced a section-header overflow at
// SetSize(9,18): the header text was truncated to the effective content width
// and then wrapped in Padding(0,1), rendering 10 cells into a 9-cell
// allocation. Every rendered line must fit the allocated width.
func TestSidebarUltraNarrowNoOverflow(t *testing.T) {
	for _, w := range []int{8, 9, 10, 11, 12} {
		s := NewSidebar(store.NewProjection())
		dir := t.TempDir()
		for i := 0; i < 12; i++ {
			inst, err := session.NewInstance(session.InstanceOptions{
				Title: fmt.Sprintf("narrow-instance-%02d", i), Path: dir, Program: "test",
			})
			require.NoError(t, err)
			addTestInstance(s, inst)
		}
		s.SetSize(w, 18)
		// Select a middle instance so tab rows and both ▲/▼ indicators are
		// inside the rendered window.
		s.SetSelectedInstance(5)
		for i, line := range strings.Split(s.String(), "\n") {
			require.LessOrEqualf(t, lipgloss.Width(line), w,
				"width=%d: line %d overflows: %q", w, i, line)
		}
	}
}

// BenchmarkSidebarTreeRender is the #1024 PR 3 synthetic-store benchmark from
// RFC §5.3: 50 instances × 9 tabs, selection mid-list (only the selected
// instance's children render — collapse-by-default bounds the row count), full
// String() per iteration at a typical sidebar allocation.
func BenchmarkSidebarTreeRender(b *testing.B) {
	s := NewSidebar(store.NewProjection())
	dir := b.TempDir()
	for i := 0; i < 50; i++ {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: fmt.Sprintf("bench-%02d", i), Path: dir, Program: "test",
		})
		if err != nil {
			b.Fatal(err)
		}
		inst.AddTabForTest("agent", session.TabKindAgent)
		inst.AddTabForTest("shell", session.TabKindShell)
		for p := 0; p < 7; p++ {
			inst.AddTabForTest(fmt.Sprintf("proc-%d", p), session.TabKindProcess)
		}
		s.proj.AddInstance(inst)
	}
	s.SetSize(48, 40)
	s.SetSelectedInstance(25)
	if got := strings.Count(s.String(), "\n") + 1; got != 40 {
		b.Fatalf("expected exactly the allocated height, got %d", got)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.String()
	}
}
