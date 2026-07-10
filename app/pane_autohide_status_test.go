package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
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
