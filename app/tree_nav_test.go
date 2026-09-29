package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// pressNav drives handleDefaultKeyPress with a mapped nav key, the way
// handleKeyPress dispatches it in stateDefault.
func pressNav(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// addTreeInstance adds an instance carrying a real agent + shell tab pair
// (the shape of a started instance after `t`) to the home's projection, so
// tree walks and tab jumps have two real slots to land on (#1100: fresh
// instances hold only the agent tab and no slot is padded).
func addTreeInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("shell", session.TabKindShell)
	h.store.AddInstance(inst)
	return inst
}

// TestTreeNav_JKWalksTabChildren pins the #1024 PR 3 / #1515 nav model at the
// app layer: j/k walk tab-to-tab, crossing instance boundaries directly from
// the last tab of one instance to the first tab of the next. Each tab row
// drives the store's active tab (the content pane binding), and h folds back
// to the parent title row.
func TestTreeNav_JKWalksTabChildren(t *testing.T) {
	h := newTestHome(t)
	a := addTreeInstance(t, h, "alpha")
	b := addTreeInstance(t, h, "beta")

	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	require.Same(t, a, h.sidebar.GetSelectedInstance())

	// j → alpha's agent tab row; j → terminal tab row. The active tab follows.
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, h.store.ActiveTab())
	assert.Equal(t, 0, h.store.ActiveTab(), "the Agent slot is active")

	pressNav(t, h, "j")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.Equal(t, 1, h.store.ActiveTab(),
		"Enter on this row would attach the terminal tab — the tab dimension routes attach")

	// j past the last child lands on beta's first tab; alpha folds
	// (collapse-by-default), and the cursor never stops on beta's title row.
	pressNav(t, h, "j")
	require.Same(t, b, h.sidebar.GetSelectedInstance())
	assert.Same(t, b, h.store.GetSelectedInstance(), "tree selection retargets the store selection")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, 0, h.store.ActiveTab())

	// k back up lands on alpha's last tab, not alpha's title row.
	pressNav(t, h, "k")
	require.Same(t, a, h.sidebar.GetSelectedInstance())
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)

	// h on a tab row folds to the parent instance row.
	require.True(t, h.sidebar.GetSelection().IsTab)
	pressNav(t, h, "h")
	sel = h.sidebar.GetSelection()
	assert.False(t, sel.IsTab, "h folds the subtree and lands on the instance row")
	assert.Same(t, a, h.sidebar.GetSelectedInstance())

	// l re-expands.
	pressNav(t, h, "l")
	pressNav(t, h, "j")
	assert.True(t, h.sidebar.GetSelection().IsTab)
}

func TestTreeNav_DownAutoOpensArchivedAtLiveBoundary(t *testing.T) {
	h := newTestHome(t)
	live := startedLocalInstance(t, "live-one")
	archived := archiveActionInstance(t, "put-away", session.Ready)
	archived.SetArchived()

	h.store.AddInstance(live)
	h.store.AddInstance(archived)
	resizeHome(h, 120, 40)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	require.Same(t, live, h.sidebar.GetSelectedInstance())
	require.NotContains(t, h.sidebar.View(), "put-away", "Archived starts collapsed")

	pressNav(t, h, "j") // live Agent tab
	pressNav(t, h, "j") // live Terminal tab
	require.True(t, h.sidebar.GetSelection().IsTab)

	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	require.Equal(t, ui.SectionArchived, sel.Kind,
		"Down after the last live tab must auto-open Archived and select the first archived row")
	require.False(t, sel.IsHeader)
	require.Same(t, archived, h.sidebar.GetSelectedInstance())
	assert.Contains(t, h.sidebar.View(), "put-away", "auto-opened Archived must render archived rows")

	pressNav(t, h, "k")
	sel = h.sidebar.GetSelection()
	require.Equal(t, ui.SectionInstances, sel.Kind)
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, live, h.sidebar.GetSelectedInstance())
}

func TestTreeNav_TabStopAcrossInstancePreservesParentForActions(t *testing.T) {
	h := newTestHome(t)
	alpha := startedLocalInstance(t, "alpha")
	beta := startedLocalInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	h.sidebar.SetSelectedInstance(0)
	h.store.SetSelectedInstance(alpha)
	_ = h.selectionChanged()

	pressNav(t, h, "j") // alpha tab 0
	pressNav(t, h, "j") // alpha tab 1
	pressNav(t, h, "j") // beta tab 0, with no beta title stop

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.ItemIndex, "tab row keeps the parent instance index")
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"instance actions resolve the selected tab's parent instance")

	var createdFor string
	t.Cleanup(SetTabCreatorForTest(func(request daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		createdFor = request.Title
		return spawnDaemonTab(beta)
	}))

	_, _ = h.createNewTab(h.sidebar.GetSelectedInstance(), session.TabKindShell)
	assert.Equal(t, beta.Title, createdFor,
		"new-tab action must target the parent session of the selected tab")
}

// TestTreeNav_NumberJumpMovesCursorOnTabRows pins the 1-9 muscle-memory rule:
// with the cursor on the instance row a number jump changes only the active
// tab (pre-tree behavior); with the cursor inside the tab subtree the cursor
// follows the jump so the tree and the tab bar agree.
func TestTreeNav_NumberJumpMovesCursorOnTabRows(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha")
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()

	// Cursor on the instance row: jump to tab 2 — cursor stays put.
	_, _ = h.handleTabJump(2)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.False(t, h.sidebar.GetSelection().IsTab, "cursor stays on the instance row")

	// Cursor on a tab row: jump moves the cursor with the active tab.
	pressNav(t, h, "j") // tab row 0 (re-selects tab 0)
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 0, h.store.ActiveTab())
	_, _ = h.handleTabJump(2)
	sel := h.sidebar.GetSelection()
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor followed the number jump")

	// Out-of-range jump stays a no-op.
	_, _ = h.handleTabJump(9)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.Equal(t, 1, h.sidebar.GetSelection().TabIndex)
}

// TestTreeNav_TabCreateCloseFromTabRow is the regression test for the PR
// #1081 play-test bug: with the cursor parked ON A TAB ROW, `t` must create
// AND select the new tab, and a following `w` must close exactly the cursor's
// tab — not a stale clamped index (the silent wrong-tab close) — and land on
// the left neighbor. The clobber came from SyncCursorToActiveTab reading
// ActiveTab() only after syncFromStore: the tab-slot change trips the
// structure rebuild, whose pushSelection re-asserts the cursor row's tab index
// over the one the handler just set.
func TestTreeNav_TabCreateCloseFromTabRow(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "tw-tab-row")
	selectInstance(h, inst)

	var closedNames []string
	t.Cleanup(SetTabCreatorForTest(func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		return spawnDaemonTab(inst)
	}))
	t.Cleanup(SetTabCloserForTest(func(request daemon.CloseTabRequest) error {
		closedNames = append(closedNames, request.TabName)
		return nil
	}))

	// Park the cursor on tab row 1 (the shell tab).
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Equal(t, 1, h.store.ActiveTab())

	// t: the new tab (index 2) must be created AND selected, cursor following.
	_, _ = h.createNewTab(h.sidebar.GetSelectedInstance(), session.TabKindShell)
	require.Equal(t, 3, inst.TabCount())
	assert.Equal(t, 2, h.store.ActiveTab(), "t from a tab row must select the new tab")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 2, sel.TabIndex, "cursor must follow onto the new tab's row")

	// w: must close exactly the cursor's tab — the fresh one — and land left.
	newTabName := inst.GetTabs()[2].Name
	_, _ = h.handleCloseTab()
	confirmTabDeletionForTest(h)
	require.Equal(t, []string{newTabName}, closedNames,
		"w from a tab row must close the cursor's tab, never a stale index")
	require.Equal(t, 2, inst.TabCount())
	assert.Equal(t, 1, h.store.ActiveTab(), "w must land on the left neighbor")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor must land on the left neighbor's row")
}

// sidebarRendersExpanded reports whether the sidebar's rendered output draws
// title's instance row with the expanded (▾) — rather than the folded (▸) —
// arrow. At the app boundary the sidebar keeps instanceExpanded private, but
// the rendered arrow is a faithful public proxy: under collapse-by-default
// only the selected instance renders ▾, and an explicit h/← collapse of the
// selection flips it back to ▸ even while it stays selected. The title is
// unique among rendered instance rows, so the substring pins that one row.
// The instance-tree arrows (▾/▸, the small triangles) are disjoint from the
// section-header arrows (▼/▲, the large ones), so the header never matches.
func sidebarRendersExpanded(h *home, title string) bool {
	return strings.Contains(h.sidebar.View(), "▾  "+title)
}

// selectTreeInstance wires the sidebar + store selection to the instance at
// idx and runs the same selectionChanged sync the event loop uses. Mirrors the
// existing app-test setup (TestTreeNav_TabStopAcrossInstancePreservesParentForActions)
// without re-adding the instance (addTreeInstance already did).
func selectTreeInstance(h *home, idx int) {
	h.sidebar.SetSelectedInstance(idx)
	h.store.SetSelectedInstance(h.store.GetInstances()[idx])
	_ = h.selectionChanged()
}

// TestTreeNav_FoldedInstanceDownLandsOnNextInstance pins the #4770 fix at the
// real key entry point the running TUI uses (handleKeyPress →
// handleDefaultKeyPress case KeyDown → sidebar.Down). The model-level tests
// exercise the sidebar directly; the batch-2 play-test (thread on #4776) could
// not confirm the head moved past a folded instance in a live TUI, and the
// review (#4776) asked for a test that drives the same entry point the TUI
// uses instead of an internal helper. dispatchKey sets keySent to reproduce
// the second pass after handleMenuHighlighting re-emits the key, so this hits
// the exact dispatch a real Down takes.
//
// Both required properties are pinned:
//   - the fold survives Down — the explicitly folded instance does NOT re-expand
//     into its own tab 0 (#4770), and it stays collapsed via collapse-by-default
//     once the selection moves on;
//   - Down moves to the next instance — the cursor lands on the next instance
//     row, that instance is selected and auto-expanded, and a following Down
//     dives into its tabs (no trap on the folded row).
func TestTreeNav_FoldedInstanceDownLandsOnNextInstance(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha")
	addTreeInstance(t, h, "bravo")
	addTreeInstance(t, h, "charlie")
	resizeHome(h, 120, 40)

	selectTreeInstance(h, 1) // bravo is the middle instance
	bravo := h.store.GetInstances()[1]
	require.Same(t, bravo, h.sidebar.GetSelectedInstance(), "bravo selected")
	require.True(t, sidebarRendersExpanded(h, "bravo"), "bravo auto-expanded")
	require.False(t, sidebarRendersExpanded(h, "alpha"), "alpha folds by default")

	// h/← folds bravo in place; the cursor stays on its row and the tab
	// children disappear.
	dispatchKey(h, runeKey('h'))
	sel := h.sidebar.GetSelection()
	require.False(t, sel.IsTab, "cursor on the folded instance row")
	require.Equal(t, 1, sel.ItemIndex, "still on bravo")
	require.False(t, sidebarRendersExpanded(h, "bravo"), "bravo folded")
	require.NotContains(t, h.sidebar.View(), "├ 1 Agent",
		"no tab rows render while the selected instance is folded")

	// Down off the folded row lands on the NEXT instance — charlie — and
	// selects it; charlie auto-expands (cross-instance auto-expand survives),
	// bravo stays folded, the cursor is not trapped. This is the #4770 fix:
	// the same instance does not re-expand.
	dispatchKey(h, runeKey('j'))
	sel = h.sidebar.GetSelection()
	require.Equal(t, 2, sel.ItemIndex, "Down moves the cursor to the next instance")
	require.False(t, sel.IsTab, "cursor lands on the next instance's row")
	require.Same(t, h.store.GetInstances()[2], h.sidebar.GetSelectedInstance(),
		"the next instance is selected")
	require.True(t, sidebarRendersExpanded(h, "charlie"),
		"the newly selected instance auto-expands")
	require.False(t, sidebarRendersExpanded(h, "bravo"),
		"the folded instance stays collapsed (collapse-by-default)")

	// A second Down dives into the now-selected instance's tabs — not trapped
	// on the row above.
	dispatchKey(h, runeKey('j'))
	sel = h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the next Down dives into the selected instance's tabs")
	require.Equal(t, 2, sel.ItemIndex, "still on charlie")
	require.Equal(t, 0, sel.TabIndex, "lands on its first tab")
}

// TestTreeNav_FoldedInstanceUpKeepsCrossInstanceExpand pins the cross-instance
// auto-expand half the review (#4776) required: after an explicit h/← fold, Up
// off a folded instance row still moves the cursor to the previous instance's
// tab and auto-expands that instance, exactly as before the fix. The Down
// side of the fix lives in its own dir>0 branch and does not touch this path
// — this pins the Up half at the same real key entry point so a future change
// to the folded-Down guard cannot silently trap the Up direction too.
func TestTreeNav_FoldedInstanceUpKeepsCrossInstanceExpand(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha")
	addTreeInstance(t, h, "bravo")
	addTreeInstance(t, h, "charlie")
	resizeHome(h, 120, 40)

	selectTreeInstance(h, 1)     // bravo
	dispatchKey(h, runeKey('h')) // fold bravo; cursor on its row
	require.False(t, sidebarRendersExpanded(h, "bravo"))

	// Up off the folded row lands on alpha's last tab and selects + expands alpha.
	dispatchKey(h, runeKey('k'))
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "Up off a folded row selects a tab of the previous instance")
	require.Equal(t, 0, sel.ItemIndex, "lands on alpha")
	require.Equal(t, 1, sel.TabIndex, "lands on alpha's last (terminal) tab")
	require.Same(t, h.store.GetInstances()[0], h.sidebar.GetSelectedInstance(), "alpha is selected")
	require.True(t, sidebarRendersExpanded(h, "alpha"), "the previous instance auto-expands")
	require.False(t, sidebarRendersExpanded(h, "bravo"),
		"the folded instance stays folded once the selection moves on")
}

// TestTreeNav_FoldedLastInstanceDownPreservesFold pins the last-instance case
// (the original #4770 no-re-expand guarantee) at the same real key entry
// point. When the folded instance is the LAST one and Down has no next row to
// move to, the move is a consumed no-op before any store mutation: the fold,
// the cursor, and the active tab all survive instead of re-expanding and
// silently resetting the active tab to 0 (the spurious-SetActiveTab case the
// model-level TestSidebarTreeCollapseDownSameInstanceKeepsActiveTab pins).
func TestTreeNav_FoldedLastInstanceDownPreservesFold(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha")
	addTreeInstance(t, h, "bravo") // the last instance
	resizeHome(h, 120, 40)

	selectTreeInstance(h, 1)
	bravo := h.store.GetInstances()[1]
	require.Same(t, bravo, h.sidebar.GetSelectedInstance())

	// Drive the active tab to the (nonzero) terminal tab before folding — a
	// spurious SetActiveTab(0) cannot be told apart from "unchanged" on the
	// default tab 0.
	h.store.SetActiveTab(1)
	require.Equal(t, 1, h.store.ActiveTab(), "active tab is the terminal tab before collapse")
	dispatchKey(h, runeKey('h')) // fold bravo
	require.False(t, sidebarRendersExpanded(h, "bravo"))
	require.False(t, h.sidebar.GetSelection().IsTab, "cursor on the folded instance row")

	// Down at the last folded instance is a consumed no-op.
	dispatchKey(h, runeKey('j'))
	sel := h.sidebar.GetSelection()
	require.False(t, sel.IsTab, "cursor does not dive into folded tabs")
	require.Equal(t, 1, sel.ItemIndex, "cursor stays on the folded last instance row")
	require.False(t, sidebarRendersExpanded(h, "bravo"),
		"the fold survives a Down with no next instance")
	require.Equal(t, 1, h.store.ActiveTab(),
		"the nonzero active tab is preserved when Down is a no-op")
}
