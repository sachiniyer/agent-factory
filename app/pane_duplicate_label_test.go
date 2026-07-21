package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPane_NumberJumpDisambiguatesDuplicateTerminalHeaders is the regression
// for #2150. The tree already prefixes each tab with its 1-based jump slot, but
// pane headers used only the non-unique presentation label. Jumping between two
// default Terminal tabs therefore rendered the same pane identity both times.
// Drive handleTabJump (the production 1-9 action) and assert on home.View so the
// test crosses the committed pane binding and TabbedWindow header render paths.
func TestPane_NumberJumpDisambiguatesDuplicateTerminalHeaders(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")
	beta.AddTabForTest("shell-2", session.TabKindShell)

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	_, _ = h.handleTabJump(3)
	require.Equal(t, 2, h.store.ActiveTab(), "tree selection is pinned to slot 3")
	pressKey(t, h, "s")
	paneB := h.store.OpenPanes()[0]
	require.Same(t, beta, paneB.Instance())
	require.Equal(t, 2, paneB.Tab())

	_, _ = h.handleTabJump(2)
	require.Equal(t, 1, paneB.Tab())
	view := h.View()
	assert.Contains(t, view, "beta · 2 › Terminal · selected: beta · 3 › Terminal",
		"the pane and tree identities remain distinct after jumping to slot 2")

	_, _ = h.handleTabJump(3)
	require.Equal(t, 2, paneB.Tab())
	view = h.View()
	assert.Contains(t, view, "beta · 3 › Terminal",
		"slot 3 is visibly distinct after the next numbered jump")
	assert.NotContains(t, view, "beta · 2 › Terminal",
		"the slot-3 frame must not retain slot 2's identity")
	assert.NotContains(t, view, "selected:",
		"the divergence hint clears once the pane rejoins the selected slot")
}
