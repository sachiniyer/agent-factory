package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// fireIdlePreviewTick drives the real 100ms preview tick through Update, so the
// tests exercise the same inPreviewTick wiring the running TUI uses.
func fireIdlePreviewTick(h *home) {
	_, _ = h.Update(previewTickMsg{})
}

// TestPane_ForwardTabVisitsAllPanesDespiteIdleTick is the #1558 regression. In
// a three-pane workspace the forward focus ring must cycle
// tree → pane → pane → pane → automations → projects → tree, visiting every
// pane and resting on the tree — even though the 100ms preview tick keeps
// firing between keystrokes. Before the fix, that idle tick yanked focus back
// onto the selected instance's already-open pane the moment the user Tabbed off
// it, so the ring never reached the other panes or settled on the tree.
func TestPane_ForwardTabVisitsAllPanesDespiteIdleTick(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes())
	require.Equal(t, 3, h.lastLayout.PaneCount(), "200 cols fits three panes")
	panes := h.store.OpenPanes()
	// The selection rests on gamma (the last opened), whose pane is rightmost.
	h.focusRegion(layout.RegionTree)

	forward := []string{
		layout.PaneRegion(panes[0].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[2].ID()),
		layout.RegionAutomations,
		layout.RegionProjects,
		layout.RegionTree,
	}
	for _, want := range forward {
		_ = h.cycleFocus(false)
		fireIdlePreviewTick(h) // the idle tick fires between keystrokes in the running TUI
		require.Equal(t, want, h.ring.Active(),
			"forward Tab + idle preview tick must advance the ring, not steal focus to the selected pane")
	}

	// Reverse mirrors it and is likewise immune to the idle tick.
	backward := []string{
		layout.RegionProjects,
		layout.RegionAutomations,
		layout.PaneRegion(panes[2].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[0].ID()),
		layout.RegionTree,
	}
	for _, want := range backward {
		_ = h.cycleFocus(true)
		fireIdlePreviewTick(h)
		require.Equal(t, want, h.ring.Active(),
			"Shift-Tab + idle preview tick must cycle the ring backwards")
	}
}

// TestPane_IdleTickDoesNotStealFocusFromTree is the tree half of #1558: while
// the ring rests on the tree with the selected instance's pane open, the idle
// preview tick must NOT pull focus onto that pane. Focus stealing on every tick
// is what broke the af_focus_tree driver helper (it could never rest on the
// tree). A real navigation still focuses an already-open tab (#1493), covered by
// TestPanePreviewSelectionFocusesAlreadyOpenTabPane.
func TestPane_IdleTickDoesNotStealFocusFromTree(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active())

	for i := 0; i < 5; i++ {
		fireIdlePreviewTick(h)
		require.Equal(t, layout.RegionTree, h.ring.Active(),
			"the idle preview tick must leave focus on the tree, not steal it to the selected pane")
	}
}
