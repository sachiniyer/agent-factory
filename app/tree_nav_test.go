package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
)

// pressNav drives handleDefaultKeyPress with a mapped nav key, the way
// handleKeyPress dispatches it in stateDefault.
func pressNav(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// addTreeInstance adds an unstarted instance (default two tab slots) to the
// home's projection.
func addTreeInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	h.store.AddInstance(inst)
	return inst
}

// TestTreeNav_JKWalksTabChildren pins the #1024 PR 3 nav model at the app
// layer: j/k walk from an instance row into its expanded tab children and out
// to the next instance, with each tab row driving the store's active tab (the
// content pane binding), and h folding back to the parent.
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
	assert.Equal(t, 0, h.store.ActiveTab(), "the agent (Preview) slot is active")

	pressNav(t, h, "j")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.Equal(t, 1, h.store.ActiveTab(),
		"Enter on this row would attach the terminal tab — the tab dimension routes attach")

	// j past the last child lands on beta; alpha folds (collapse-by-default).
	pressNav(t, h, "j")
	require.Same(t, b, h.sidebar.GetSelectedInstance())
	assert.Same(t, b, h.store.GetSelectedInstance(), "tree selection retargets the store selection")

	// k back up: beta's row was preceded by alpha's (now folded) row.
	pressNav(t, h, "k")
	require.Same(t, a, h.sidebar.GetSelectedInstance())

	// h on a tab row folds to the parent instance row.
	pressNav(t, h, "j")
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
	t.Cleanup(SetTabCreatorForTest(func(title, repoID string) (string, error) {
		return nextShellTabName(inst.GetTabs()), nil
	}))
	t.Cleanup(SetTabCloserForTest(func(title, repoID, tabName string) error {
		closedNames = append(closedNames, tabName)
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
	_, _ = h.handleNewTab()
	require.Equal(t, 3, inst.TabCount())
	assert.Equal(t, 2, h.store.ActiveTab(), "t from a tab row must select the new tab")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 2, sel.TabIndex, "cursor must follow onto the new tab's row")

	// w: must close exactly the cursor's tab — the fresh one — and land left.
	newTabName := inst.GetTabs()[2].Name
	_, _ = h.handleCloseTab()
	require.Equal(t, []string{newTabName}, closedNames,
		"w from a tab row must close the cursor's tab, never a stale index")
	require.Equal(t, 2, inst.TabCount())
	assert.Equal(t, 1, h.store.ActiveTab(), "w must land on the left neighbor")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor must land on the left neighbor's row")
}
