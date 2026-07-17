package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
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

// openPanesForBothAndReturnHidden opens alpha's and beta's panes at a width
// where only one fits, then returns (visible instance, hidden instance) for the
// resulting split — which pane wins visibility is a store detail the callers
// don't care about, only that exactly one is hidden.
func openPanesForBothAndReturnHidden(t *testing.T, h *home) (visible, hidden *session.Instance) {
	t.Helper()
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes())
	require.Equal(t, 1, len(h.visiblePanes), "only one pane fits at the narrow width")
	visible = h.visiblePanes[0].Instance()
	hidden = alpha
	if visible == alpha {
		hidden = beta
	}
	return visible, hidden
}

// TestPane_AutoHideViaPreviewFocusStartsClearTimer is the #1685 regression:
// landing on an already-open-but-hidden pane through the mouse/preview path
// (selectionChanged → updatePanePreview → focusOpenPane) auto-hides the visible
// pane via relayout. That notice must start its 3s auto-clear timer just like
// the resize and open-or-focus paths do — the bug left updatePanePreview's
// focusOpenPane relayout setting the status but never consuming it, so the
// "N hidden" guidance lingered forever. updatePanePreview now consumes it and
// returns the clear-timer cmd, which selectionChanged batches to the event loop.
func TestPane_AutoHideViaPreviewFocusStartsClearTimer(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // only one pane fits

	_, hidden := openPanesForBothAndReturnHidden(t, h)

	// Reset any notice state the open path left behind so the assertion isolates
	// the preview-focus relayout.
	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Drive the mouse/preview path directly: landing on the hidden instance
	// focuses its pane, re-hiding the visible one and showing the auto-hide
	// notice — which must be consumed so the timer runs.
	cmd := h.updatePanePreview(hidden, 0, false, false)

	require.NotEmpty(t, h.errBox.FullError(), "the auto-hide notice is shown on re-hide")
	assert.Equal(t, "", h.pendingPaneAutoHideStatus,
		"the preview-focus path consumes the pending status instead of leaving it dangling (#1685)")
	require.NotNil(t, cmd,
		"updatePanePreview returns the clear-timer cmd so the notice auto-clears after 3s (#1685)")
}

// TestPane_OpenFocusRefreshProducedStatusStillClears guards the Greptile edge on
// PR #1771: a status produced by the SUBSEQUENT selectionChanged refresh during
// an open-or-focus must still be drained + timed. Here focusOpenPane on the
// already-visible pane hides nothing, but the refresh reads the sidebar cursor
// (parked on the hidden instance), re-focuses that pane, and auto-hides the
// visible one — producing the status LATE. openOrFocusPane's consume runs AFTER
// selectionChanged precisely so that late status never lingers with no timer.
func TestPane_OpenFocusRefreshProducedStatusStillClears(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // only one pane fits

	visible, hidden := openPanesForBothAndReturnHidden(t, h)

	// Park the sidebar cursor on the hidden instance so the refresh inside the
	// next openOrFocusPane re-focuses it (and auto-hides the visible pane).
	hiddenIdx := 0
	for i, inst := range h.store.GetInstances() {
		if inst == hidden {
			hiddenIdx = i
		}
	}
	h.sidebar.SetSelectedInstance(hiddenIdx)

	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Open/focus the currently-visible instance. Its own focusOpenPane hides
	// nothing; the auto-hide status is produced by the refresh that follows.
	_, cmd := h.openOrFocusPane(visible, 0)

	require.NotEmpty(t, h.errBox.FullError(), "the refresh-produced auto-hide notice is shown")
	assert.Equal(t, "", h.pendingPaneAutoHideStatus,
		"a status produced by the refresh during open/focus is still drained (#1685)")
	require.NotNil(t, cmd,
		"openOrFocusPane returns a clear-timer cmd so the late notice auto-clears (#1685)")
}

// TestPane_AutoHideStatusNamesDisplacedTab is the #1997 regression, at the exact
// size the play-test found it (80x24, where only one pane fits). Two panes are
// open on the SAME instance — `alpha · Agent` and `alpha · Terminal` — so the
// instance title alone names BOTH of them and cannot say which one went away.
// Naming only the instance made the toast read "alpha hidden" while
// `alpha · Terminal` was visibly on screen: the UI contradicting what the user
// can see. Since #930 a pane is identified by instance AND tab, so the toast
// must name the displaced TAB.
//
// The assertion is on the RENDERED status line — the clipped text the user
// actually reads — because FullError() is the un-truncated string we passed in
// and never shows what the 80-column bar does to it (#1973).
func TestPane_AutoHideStatusNamesDisplacedTab(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, 80, 24) // the play-test size: below MultiPaneMinWidth, one pane fits

	alpha := h.store.GetInstanceByTitle("alpha")
	require.NotNil(t, alpha)

	// Open alpha · Agent (tab 0), then alpha · Terminal (tab 1). The second pane
	// on the same instance displaces the first.
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(alpha, 1)

	require.Equal(t, 2, h.store.NumOpenPanes(), "both of alpha's tabs are open as panes")
	require.Equal(t, 1, len(h.visiblePanes), "only one pane fits at 80 columns")
	require.Equal(t, 1, h.visiblePanes[0].Tab(),
		"alpha · Terminal is the pane left visible, so alpha · Agent is the displaced one")

	rendered := h.errBox.String()
	assert.Contains(t, rendered, "alpha · Agent hidden",
		"the toast names the tab that was actually displaced (#1997)")
	assert.Contains(t, rendered, "resize wider",
		"naming the tab must not push the recovery hint off the 80-column bar (#1973)")
}

// TestPane_AutoHideStatusUnnamableTabMakesNoClaim covers the other half of the
// #1997 contract: when the pane's tab cannot be named, the toast says a pane is
// hidden rather than naming the wrong one. An instance whose tabs have not
// materialized yet has no real tab list to read, and tree.TabLabels answers
// with the placeholder "Agent" slot — so naming the tab from it would present a
// guess as a fact. The toast must decline to name instead.
func TestPane_AutoHideStatusUnnamableTabMakesNoClaim(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, 80, 24)

	// An instance with no materialized tabs: nothing can say what its tab 0 is.
	blank := instanceWithFakeBackend(t, "blank")
	h.store.AddInstance(blank)
	require.Empty(t, blank.GetTabs(), "the fixture instance has no materialized tabs")

	alpha := h.store.GetInstanceByTitle("alpha")
	require.NotNil(t, alpha)
	_, _ = h.openOrFocusPane(blank, 0)
	_, _ = h.openOrFocusPane(alpha, 0)

	rendered := h.errBox.String()
	assert.Contains(t, rendered, "a pane is hidden",
		"an unnamable pane is reported without a name (#1997)")
	assert.NotContains(t, rendered, "blank · Agent",
		"the placeholder tab slot must never be presented as the displaced tab's real name")
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
