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
	"github.com/sachiniyer/agent-factory/ui/tree"
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

// TestSidebarTreeCollapseSurvivesDownFromInstanceRow pins the #4770 regression
// under the corrected navigation: a single Down off an explicitly-collapsed
// (h/←) instance row must NOT re-expand the SAME instance into its own tab 0.
// Down now moves the cursor off the folded row to the next instance instead of
// trapping it; the field's doc states treeCollapsed is "Cleared when the
// selection moves to a different instance", and once the cursor moves on the
// folded instance stays collapsed via collapse-by-default. The pre-fix
// unconditional s.treeCollapsed = "" in selectTabStop reverted the collapse;
// the no-op guard that replaced it then trapped the cursor so j/Down could
// never move past a folded instance.
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

	// A single Down must move the cursor off the folded row to the next
	// instance, not dive into instance 0's folded tabs and not stay trapped.
	s.Down()
	sel = s.GetSelection()
	assert.Equal(t, 1, sel.ItemIndex, "Down moves the cursor to the next instance")
	assert.False(t, sel.IsTab, "cursor lands on the next instance's row")

	// #4770: the folded instance must NOT re-expand — its tab children stay
	// hidden once the cursor has moved on.
	instances := s.proj.GetInstances()
	require.Equal(t, "t-00", instances[0].Title)
	assert.False(t, s.instanceExpanded(instances[0]),
		"the folded instance does not re-expand on Down")
}

// TestSidebarTreeCollapseDownSameInstanceKeepsActiveTab pins the active-tab
// preservation half of the fix for the LAST folded instance: when a Down off a
// folded instance has no next instance to move to, the move is a consumed
// no-op before any store mutation, so SetActiveTab is not called and the
// nonzero active tab is not silently reset to 0. (The multi-instance case now
// moves the cursor to the next instance, covered by
// TestSidebarTreeCollapseDownLandsOnNextInstance; only the last instance has
// nowhere to go.) The sibling scenario below uses the default active tab (0),
// which a spurious SetActiveTab(0) cannot be told apart from "unchanged", so
// this one drives the active tab to the nonzero terminal tab before folding.
func TestSidebarTreeCollapseDownSameInstanceKeepsActiveTab(t *testing.T) {
	s := newTreeSidebar(t, 1) // a single folded instance: Down has nowhere to go
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "instance 0 auto-expanded")

	// Drive the active tab to the (nonzero) terminal tab, then fold the
	// instance in place from its row.
	s.proj.SetActiveTab(1)
	require.Equal(t, 1, s.proj.ActiveTab(), "active tab is the terminal tab before collapse")
	s.CollapseSection()
	require.Equal(t, "t-00", s.treeCollapsed)
	require.Equal(t, 0, tabRowCount(s))
	require.False(t, s.GetSelection().IsTab, "collapse leaves the cursor on the instance row")

	// Down at the last folded instance is a consumed no-op: the fold, cursor
	// and active tab all survive.
	s.Down()
	assert.False(t, s.GetSelection().IsTab, "cursor does not dive into folded tabs")
	assert.Equal(t, 0, s.GetSelection().ItemIndex, "cursor stays on the folded instance row")
	assert.Equal(t, "t-00", s.treeCollapsed, "explicit collapse survives a Down with no next instance")
	assert.Equal(t, 1, s.proj.ActiveTab(), "active tab preserved when Down is a no-op")
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
// finding's regression (review 5274299199): a Down off the Instances header
// must clear the sticky collapse override and select the target tab
// normally. A second h/← from the collapsed-instance row jumps to the
// Instances header and folds the section, but pushSelection skips header
// rows, so the store's sticky selection and treeCollapsed both still name the
// just-collapsed instance while the cursor is on the header. Down from the
// header reaches selectTabStop for that still-sticky instance, which clears
// the override and re-expands it so the target tab can be revealed and
// selected. Navigation from a header must not be consumed by the folded-row
// behavior, which is why the folded-instance handling lives in
// tryMoveVerticalNavStop and keys off the cursor resting on the instance row.
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

// TestSidebarTreeCollapseDownLandsOnNextInstance pins the navigation fix the
// batch play-test (see #4745) required: a Down off an explicitly folded (h/←)
// instance lands on the NEXT instance — the display binding follows the
// cursor, so that next instance is selected and auto-expands while the folded
// one stays collapsed — and a following Down keeps moving forward instead of
// trapping on the folded row.
func TestSidebarTreeCollapseDownLandsOnNextInstance(t *testing.T) {
	s := newTreeSidebar(t, 3) // instances t-00, t-01, t-02
	s.SetSelectedInstance(0)
	s.CollapseSection()
	require.Equal(t, "t-00", s.treeCollapsed)
	require.False(t, s.GetSelection().IsTab, "cursor on the folded instance row")

	// Down lands on the next instance row and selects it; the folded one
	// stays collapsed and the override clears once the selection moves on.
	s.Down()
	require.Equal(t, "t-01", s.GetSelectedInstance().Title,
		"Down from a folded instance selects the next instance")
	sel := s.GetSelection()
	assert.Equal(t, 1, sel.ItemIndex, "cursor lands on the next instance row")
	assert.False(t, sel.IsTab, "cursor rests on the instance row, not a tab")
	assert.False(t, s.instanceExpanded(s.proj.GetInstances()[0]),
		"the folded instance stays collapsed")
	assert.Equal(t, "", s.treeCollapsed, "the override clears once the selection moves on")
	assert.Equal(t, 2, tabRowCount(s), "the next instance auto-expands")

	// A second Down proceeds normally into the now-selected instance's tabs —
	// the cursor is not trapped on the row above.
	s.Down()
	sel = s.GetSelection()
	assert.True(t, sel.IsTab, "the next Down dives into the selected instance's tabs")
	assert.Equal(t, 1, sel.ItemIndex, "still on the next instance")
	assert.Equal(t, 0, sel.TabIndex, "lands on its first tab")
}

// TestSidebarTreeCollapseDownSkipsRootSeparator pins the P1 inline Codex
// finding (review 5385343335): when the reserved root agent is the explicitly
// folded (h/←) row and a non-root instance follows it, rebuildVisibleItems
// inserts an IsRootSep hairline (ItemIndex == -1, a SectionInstances item that
// is neither a header nor a tab) immediately below the root. moveCursorToNext
// InstanceRow must skip that decorative row — the pre-fix predicate accepted
// it, set the cursor to its ItemIndex == -1 row, and reported success; at the
// app boundary that resolved to no instance, and a second Down then targeted the
// root's hidden tab and re-expanded the fold (#4770 regression). Down off the
// folded root must land on the next real instance row instead.
func TestSidebarTreeCollapseDownSkipsRootSeparator(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	dir := t.TempDir()
	for _, title := range []string{"root", "alpha"} {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: dir, Program: "test"})
		require.NoError(t, err)
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	s.SetSize(40, 24)

	// The reserved root sorts first (LessInstanceOrder), so it is index 0; a
	// root+non-root pair is exactly the shape that emits the IsRootSep row.
	require.True(t, session.IsReservedTitle(s.proj.GetInstances()[0].Title), "root is index 0")
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "root auto-expanded")

	// h/← folds the root in place; the cursor stays on its row and the tab
	// children disappear. The IsRootSep hairline is still emitted below it.
	s.CollapseSection()
	require.Equal(t, "root", s.treeCollapsed)
	require.Equal(t, 0, tabRowCount(s))
	require.False(t, s.GetSelection().IsTab, "cursor on the folded root row")

	// Down must NOT land on the IsRootSep row (ItemIndex -1): it moves past it
	// to the next real instance row, selecting alpha. The root stays folded
	// (#4770) and the override clears once the selection moves on.
	s.Down()
	sel := s.GetSelection()
	assert.NotEqual(t, -1, sel.ItemIndex, "Down must not land on the IsRootSep hairline")
	assert.False(t, sel.IsTab, "cursor lands on the next instance row")
	assert.Equal(t, "alpha", s.GetSelectedInstance().Title, "Down selects the next real instance")
	assert.False(t, s.instanceExpanded(s.proj.GetInstances()[0]),
		"the folded root does not re-expand")
	assert.Equal(t, "", s.treeCollapsed, "the override clears once the selection moves on")
}

// TestSidebarTreeCollapseDownSkipsInFlightInstance pins the P2 inline Codex
// finding (review 5385343335): when the next live instance after a folded one
// is mid-op (in-flight, non-expandable), normal vertical nav (liveTabStops)
// excludes its row, so a Down off a folded predecessor must not retarget
// selection onto the transient title either. moveCursorToNextInstanceRow now
// skips non-expandable instance rows and advances to the next expandable
// instance (or, if there is none, reports no move so the reveal-Archived
// fallback takes over). Here instance 1 is in-flight, so Down off folded
// instance 0 skips it and lands on instance 2.
func TestSidebarTreeCollapseDownSkipsInFlightInstance(t *testing.T) {
	s := newTreeSidebar(t, 3) // t-00, t-01, t-02
	s.SetSelectedInstance(0)
	require.Equal(t, 2, tabRowCount(s), "instance 0 auto-expanded")

	// Make the next instance (t-01) non-expandable with an in-flight op — the
	// same transient shape liveTabStops skips during normal j/k navigation.
	middle := s.proj.GetInstances()[1]
	middle.SetInFlightOpForTest(session.OpKilling)
	require.False(t, tree.Expandable(middle), "precondition: t-01 is non-expandable")

	// h/← folds instance 0 in place.
	s.CollapseSection()
	require.Equal(t, "t-00", s.treeCollapsed)
	require.False(t, s.GetSelection().IsTab, "cursor on the folded instance row")

	// Down must skip the in-flight t-01 row and land on the next expandable
	// instance (t-02), not retarget selection onto the transient session.
	s.Down()
	sel := s.GetSelection()
	assert.Equal(t, 2, sel.ItemIndex, "Down skips the in-flight instance and lands on t-02")
	assert.False(t, sel.IsTab, "cursor lands on the next expandable instance row")
	assert.Equal(t, "t-02", s.GetSelectedInstance().Title, "Down selects the next expandable instance")
	assert.False(t, s.instanceExpanded(s.proj.GetInstances()[0]),
		"the folded instance stays collapsed")
	assert.Equal(t, "", s.treeCollapsed, "the override clears once the selection moves on")
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
