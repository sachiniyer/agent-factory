package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Equal(t, ui.ContentModeInstance, h.contentPane.GetMode())

	// j → alpha's agent tab row; j → terminal tab row. The active tab follows.
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, h.store.ActiveTab())
	assert.True(t, h.contentPane.TabbedWindow().IsInPreviewTab())

	pressNav(t, h, "j")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.True(t, h.contentPane.TabbedWindow().IsInTerminalTab(),
		"Enter on this row would attach the terminal tab — the tab dimension routes attach")

	// j past the last child lands on beta; alpha folds (collapse-by-default).
	pressNav(t, h, "j")
	require.Same(t, b, h.sidebar.GetSelectedInstance())
	assert.Same(t, b, h.store.GetSelectedInstance(), "tree selection retargets the content pane binding")

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
