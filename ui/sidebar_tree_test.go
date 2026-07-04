package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
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
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
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
// same labels (and 1-based numbers) as the tab bar, the active tab carries the
// tmux-style "*" marker, and non-selected instances stay collapsed with a ▸.
func TestSidebarTreeRendersTabChildren(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSize(40, 24)

	// Nothing selected yet: no tab rows, both instances collapsed.
	out := s.String()
	assert.NotContains(t, out, "├", "no tab children before a selection exists")

	s.SetSelectedInstance(0)
	out = s.String()
	assert.Contains(t, out, "├ 1 Preview *", "agent tab child with slot number and active marker")
	assert.Contains(t, out, "└ 2 Terminal", "terminal tab child with └ terminator")
	assert.Contains(t, out, "▾", "selected instance shows the expanded arrow")
	assert.Contains(t, out, "▸", "non-selected instance stays collapsed")
	assert.Equal(t, 2, tabRowCount(s), "only the selected instance contributes tab rows")

	// The active-tab marker follows the store's active tab.
	s.proj.SetActiveTab(1)
	out = s.String()
	assert.Contains(t, out, "└ 2 Terminal *")
	assert.NotContains(t, out, "├ 1 Preview *")
}

// TestSidebarTreeFreshInstanceSingleTabRow pins the #1100 tree rendering: a
// fresh instance holds only its agent tab, so its expanded subtree is exactly
// one child row — no phantom "Terminal" row for a tab that doesn't exist —
// and the on-demand shell tab (`t`) grows it to two.
func TestSidebarTreeFreshInstanceSingleTabRow(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fresh", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.AddTabForTest("agent", session.TabKindAgent)
	addTestInstance(s, inst)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)

	require.Equal(t, 1, tabRowCount(s), "fresh instance: exactly one tab row")
	out := s.String()
	assert.Contains(t, out, "└ 1 Preview *", "the agent tab is the only — and last — child row")
	assert.NotContains(t, out, "Terminal", "no phantom Terminal row before t is pressed")

	// `t` materializes the shell tab; the tree grows a real second row.
	inst.AddTabForTest("shell", session.TabKindShell)
	assert.Equal(t, 2, tabRowCount(s), "after t: the on-demand terminal is the second row")
	assert.Contains(t, s.String(), "└ 2 Terminal")
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

	// Simulate handleNewTab: the instance grows a third slot in place (no
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
	inst.SetStatus(session.Deleting)
	// Status flips in place (no store version bump) — the structure signature
	// must still pick it up on the next read.
	assert.Equal(t, 0, tabRowCount(s), "deleting instance folds its tab children")
	out := s.String()
	assert.Contains(t, out, "[deleting]")
	assert.NotContains(t, out, "├", "no tab children while deleting")
	assert.NotContains(t, out, "▾", "no expanded arrow while deleting")

	inst.SetStatus(session.Ready)
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
	assert.Contains(t, s.String(), "└ 3 btop")
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
	assert.Contains(t, out, "└ 2 Terminal", "selected tab row must be inside the window")
}

// TestSidebarUltraNarrowNoOverflow pins the #646 no-overflow guarantee for
// EVERY sidebar row kind at ultra-narrow allocations — section headers, the
// title bar (both auto-yes chips), instance rows, tab rows, and the ▲/▼
// window indicators. Greptile/T-Rex reproduced a section-header overflow at
// SetSize(9,18): the header text was truncated to the effective content width
// and then wrapped in Padding(0,1), rendering 10 cells into a 9-cell
// allocation. Every rendered line must fit the allocated width.
func TestSidebarUltraNarrowNoOverflow(t *testing.T) {
	for _, autoyes := range []bool{false, true} {
		for _, w := range []int{8, 9, 10, 11, 12} {
			spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
			s := NewSidebar(&spin, autoyes, store.NewProjection())
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
					"autoyes=%v width=%d: line %d overflows: %q", autoyes, w, i, line)
			}
		}
	}
}

// BenchmarkSidebarTreeRender is the #1024 PR 3 synthetic-store benchmark from
// RFC §5.3: 50 instances × 9 tabs, selection mid-list (only the selected
// instance's children render — collapse-by-default bounds the row count), full
// String() per iteration at a typical sidebar allocation.
func BenchmarkSidebarTreeRender(b *testing.B) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
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
