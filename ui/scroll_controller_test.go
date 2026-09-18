package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/stretchr/testify/require"
)

func TestHostHistoryScrollControllerConformanceAtTerminalSizes(t *testing.T) {
	history := numberedScrollHistory(100)

	for _, size := range []struct {
		name          string
		width, height int
	}{
		{name: "80x24", width: 80, height: 24},
		{name: "120x40", width: 120, height: 40},
	} {
		t.Run(size.name, func(t *testing.T) {
			v := viewport.New(size.width, size.height)
			controller := newHostHistoryScrollController()
			require.Equal(t, ScrollOwnerHostHistory, controller.Owner())

			controller.Scroll(&v, scrollOneLineUp)
			require.True(t, controller.Active())
			require.True(t, controller.NeedsFill(size.height))
			token, claimed := controller.ClaimFill()
			require.True(t, claimed)
			require.True(t, controller.CompleteFill(token, &v, history))

			bottom := 100 - size.height
			require.Equal(t, bottom-1, v.YOffset,
				"first intent must land one row above bottom at %s", size.name)

			controller.Scroll(&v, scrollOneLineDown)
			require.Equal(t, bottom, v.YOffset,
				"down intent must return to the newest host-history row")
		})
	}
}

func TestHostHistoryScrollControllerPreservesIntentAcrossPendingResize(t *testing.T) {
	history := numberedScrollHistory(100)
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)

	// The real TUI can resize while capture is in flight. The eventual offset is
	// computed from the new geometry, while both pre/post-resize intents survive.
	controller.Resize(&v, 120, 40)
	controller.Scroll(&v, scrollOneLineUp)
	require.True(t, controller.CompleteFill(token, &v, history))
	require.Equal(t, 58, v.YOffset, "120x40 bottom 60 minus two queued up intents")
}

func TestHostHistoryScrollControllerReplaysQueuedIntentInOrder(t *testing.T) {
	history := numberedScrollHistory(100)
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	// A down request at the newest row clamps there; the subsequent up request
	// must still move one row. Reducing the queue to a net displacement would
	// incorrectly cancel the two and lose their ordering semantics.
	controller.Scroll(&v, scrollOneLineDown)
	controller.Scroll(&v, scrollOneLineUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)
	require.True(t, controller.CompleteFill(token, &v, history))
	require.Equal(t, 75, v.YOffset)
}

func TestHostHistoryScrollControllerPreservesDistanceAcrossReadyResize(t *testing.T) {
	history := numberedScrollHistory(100)
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)
	require.True(t, controller.CompleteFill(token, &v, history))
	require.Equal(t, 75, v.YOffset, "80x24 bottom 76 minus one")

	controller.Resize(&v, 120, 40)
	require.Equal(t, 59, v.YOffset, "120x40 bottom 60 minus the same one-row distance")
	controller.Resize(&v, 80, 24)
	require.Equal(t, 75, v.YOffset, "shrinking must preserve distance from bottom too")
}

// TestHostHistoryScrollControllerHalfPageIntent pins #4173: ctrl+u/ctrl+d are
// the conventional half-page keys (vim, less, tmux copy-mode), so the intent
// must displace half the viewport's height — not the wheel's one line.
func TestHostHistoryScrollControllerHalfPageIntent(t *testing.T) {
	history := numberedScrollHistory(100)
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollHalfPageUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)
	require.True(t, controller.CompleteFill(token, &v, history))
	require.Equal(t, 76-12, v.YOffset, "80x24 bottom 76 minus half a page")

	controller.Scroll(&v, scrollHalfPageDown)
	require.Equal(t, 76, v.YOffset, "ctrl+d moves half a page toward newer")
}

// TestHostHistoryScrollControllerHalfPageTracksPendingResize keeps the
// displacement semantic across the asynchronous fill: a ctrl+u queued while a
// resize is in flight resolves against the geometry it lands on, not the height
// the pane had when the key was pressed.
func TestHostHistoryScrollControllerHalfPageTracksPendingResize(t *testing.T) {
	history := numberedScrollHistory(100)
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollHalfPageUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)

	controller.Resize(&v, 80, 40)
	require.True(t, controller.CompleteFill(token, &v, history))
	require.Equal(t, 60-20, v.YOffset, "80x40 bottom 60 minus half of 40, not half of 24")
}

// TestResolveScrollLinesFloorsAtOne: half of a one-row viewport still moves —
// a semantic intent must never resolve to a no-op.
func TestResolveScrollLinesFloorsAtOne(t *testing.T) {
	v := viewport.New(80, 1)
	require.Equal(t, -1, resolveScrollLines(&v, scrollHalfPageUp))
	require.Equal(t, 1, resolveScrollLines(&v, scrollHalfPageDown))
}

// TestHostHistoryScrollFillMarksCutRows pins the scroll-mode half of #4175:
// the viewport's own View() truncates each visible line at Width before any
// render-time clamp can see it, so a row wider than the box must be marked on
// the raw fill — otherwise scroll mode shows a silent hard cut where the
// preview would show "…".
func TestHostHistoryScrollFillMarksCutRows(t *testing.T) {
	v := viewport.New(10, 4)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)
	require.True(t, controller.CompleteFill(token, &v,
		"short\nabcdefghijklmnop\nfits-10col"))

	lines := strings.Split(v.View(), "\n")
	require.Equal(t, "abcdefghi…", lines[1],
		"an over-wide history row is marked at fill, before View() can cut it")
	require.Equal(t, "fits-10col", lines[2],
		"a row exactly at the box width is not mistaken for a cut one")
	require.NotContains(t, lines[0], "…")
}

// TestHostHistoryScrollResizeRemarksCutRows: a marker stamped for the fill-time
// width would hard-cut away in a narrower box and sit stale mid-row in a wider
// one, so resize re-marks the retained raw capture.
func TestHostHistoryScrollResizeRemarksCutRows(t *testing.T) {
	v := viewport.New(10, 4)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	token, claimed := controller.ClaimFill()
	require.True(t, claimed)
	require.True(t, controller.CompleteFill(token, &v, "abcdefghijklmnop"))

	controller.Resize(&v, 6, 4)
	require.Equal(t, "abcde…", strings.Split(v.View(), "\n")[0],
		"the cut row is re-marked at the narrower width, not hard-cut at six")

	controller.Resize(&v, 14, 4)
	require.Equal(t, "abcdefghijklm…",
		strings.Split(v.View(), "\n")[0],
		"growing re-marks the raw row rather than leaving the stale marker")
}

func TestPassiveScrollControllersNeverEnterHostHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		owner      ScrollOwner
		controller func() ScrollController
	}{
		{name: "child application", owner: ScrollOwnerChildApplication, controller: newChildApplicationScrollController},
		{name: "unknown", owner: ScrollOwnerNone, controller: newUnavailableScrollController},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := viewport.New(80, 24)
			controller := tc.controller()
			controller.Scroll(&v, scrollOneLineUp)
			require.Equal(t, tc.owner, controller.Owner())
			require.False(t, controller.Active())
			require.Equal(t, 0, v.YOffset)
		})
	}
}

func TestUnknownScrollOwnerFailsLoudly(t *testing.T) {
	p := NewTabPane(nil)
	require.PanicsWithValue(t, "ui: unknown scroll owner 255", func() {
		p.SetScrollOwnerFor(nil, 0, ScrollOwner(255))
	})
}
