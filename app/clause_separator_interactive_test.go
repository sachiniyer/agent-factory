package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPaneHeaderInteractiveCueStaysInsideBoundIdentity is the interactive-mode
// companion to TestPaneHeaderSetsOffItsClausesWithADash. That test pins the
// `— selected:` clause boundary for a divergent selection alone; this one adds
// interactive mode on top. The `· keyboard` cue is an identity fragment (joined
// with ` · `, #2579), so it must stay INSIDE the bound pane's identity — before
// the `— selected:` clause — rather than landing at the end of the header where
// the append used to drop it inside the selected session's clause
// (`alpha · Agent — selected: beta · Agent · keyboard`).
func TestPaneHeaderInteractiveCueStaysInsideBoundIdentity(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	// Move the tree cursor onto beta and dismiss the resulting preview, leaving
	// paneA bound to (alpha, 0) with the store selection on beta — the same
	// divergent-selection state TestPaneHeaderSetsOffItsClausesWithADash pins.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())
	h.cancelPanePreview(false)

	view := h.View()
	require.Contains(t, view, "alpha · Agent — selected: beta · Agent",
		"precondition: the divergent-selection clause is set off with a dash")

	// Entering interactive mode does not snap the store selection to the pane,
	// so the selection stays on beta and the per-View selectionHint stays
	// non-empty; updatePanePreview short-circuits while interactive
	// (pane_preview.go), so renderHeader runs its bound-instance arm with no
	// preview. The model's interactive entry is driven directly so the repro is
	// hermetic without a live tmux attachment — renderHeader reads w.interactive
	// and w.selectionHint, both of which setInteractive / paneSelectionHint set.
	require.Same(t, paneA, h.focusedOpenPane(), "precondition: the opened pane is focused")
	h.setInteractive(true)
	require.True(t, h.paneWindows[paneA.ID()].Interactive(),
		"the bound pane must carry the keyboard cue")

	view = h.View()
	assert.Contains(t, view, "alpha · Agent · keyboard — selected: beta · Agent",
		"the keyboard cue is a fragment of the bound identity and must precede the selected clause")
	assert.NotContains(t, view, "alpha · Agent — selected: beta · Agent · keyboard",
		"the cue must not land inside the selected session's clause")
}
