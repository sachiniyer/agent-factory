package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPane_OpenHiddenThenResizeWideNoDupNoStaleStatus is the #1557 regression:
// open every instance's pane while too narrow to fit them all (earlier panes
// auto-hide), then resize wide enough for all of them. The workspace must show
// exactly one pane per instance — no duplicate for a revealed pane — and the
// "N hidden: terminal too narrow" guidance must clear once the panes fit again,
// never linger contradicting the now-visible panes.
func TestPane_OpenHiddenThenResizeWideNoDupNoStaleStatus(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // one pane fits

	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes(), "each instance opens exactly one pane")
	require.Equal(t, 1, len(h.visiblePanes), "only one pane fits at the narrow width")
	require.NotEmpty(t, h.errBox.FullError(), "an auto-hide status is shown while panes are hidden")

	// Grow wide enough to fit all three panes.
	resizeHome(h, 200, 50)

	assert.Equal(t, 3, h.store.NumOpenPanes(), "no duplicate pane is created on reveal")
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h),
		"every instance is shown by exactly one pane, in order")
	assert.Equal(t, "", h.errBox.FullError(),
		"the narrow-width guidance clears once the panes fit again")
}

// TestPane_AutoHideViaFocusOpenPaneStartsClearTimer is the #1685 regression:
// focusing an already-open pane through the mouse/preview path (focusOpenPane,
// reached via selectionChanged → updatePanePreview) can auto-hide another pane
// via relayout. That notice must start its 3s auto-clear timer just like the
// resize and open-or-focus paths do — the bug left focusOpenPane's relayout
// setting the status but never consuming it, so the "N hidden" guidance
// lingered forever. focusOpenPane now returns the consume cmd; the pending
// status is drained and a clear-timer cmd is handed back.
func TestPane_AutoHideViaFocusOpenPaneStartsClearTimer(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // only one pane fits

	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)

	// Open both instances' panes; at this width only one fits, so exactly one is
	// visible and the other is auto-hidden.
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes())
	require.Equal(t, 1, len(h.visiblePanes), "only one pane fits at the narrow width")

	// Grab the currently hidden pane — focusing it will re-hide the visible one.
	visibleID := h.visiblePanes[0].ID()
	var hidden *store.OpenPane
	for _, p := range h.store.OpenPanes() {
		if p.ID() != visibleID {
			hidden = p
		}
	}
	require.NotNil(t, hidden, "one pane is auto-hidden at the narrow width")

	// Reset any notice state the open path left behind so the assertion isolates
	// the focusOpenPane relayout.
	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Focus the hidden pane the way the mouse/preview path does. This re-hides
	// the other pane, showing the auto-hide notice — which must be consumed so
	// the timer runs.
	cmd := h.focusOpenPane(hidden)

	require.NotEmpty(t, h.errBox.FullError(), "the auto-hide notice is shown on re-hide")
	assert.Equal(t, "", h.pendingPaneAutoHideStatus,
		"focusOpenPane consumes the pending status instead of leaving it dangling (#1685)")
	require.NotNil(t, cmd,
		"focusOpenPane returns the clear-timer cmd so the notice auto-clears after 3s (#1685)")
}

// TestPane_OpenPaneWindowIsIdempotent proves the (instance, tab) →
// at-most-one-pane invariant is enforced at the openPaneWindow chokepoint, so
// the callers that skip the FindOpenPane pre-check (auto-open, restore) cannot
// split one tab across two panes (#1557).
func TestPane_OpenPaneWindowIsIdempotent(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	first := h.openPaneWindow(alpha, 0)
	require.NotNil(t, first)
	second := h.openPaneWindow(alpha, 0)
	require.Same(t, first, second, "re-opening the same (instance, tab) returns the existing pane")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "no duplicate pane is appended")
}
