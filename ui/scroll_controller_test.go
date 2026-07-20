package ui

import (
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"
)

func TestHostHistoryScrollControllerConformanceAtTerminalSizes(t *testing.T) {
	history := lipgloss.JoinVertical(lipgloss.Left, numberedScrollHistory(100), scrollFooter())

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
			gen := controller.FillGeneration()
			require.True(t, controller.CompleteFill(gen, &v, history))

			bottom := 101 - size.height // 100 history rows + one footer row.
			require.Equal(t, bottom-1, v.YOffset,
				"first intent must land one row above bottom at %s", size.name)

			controller.Scroll(&v, scrollOneLineDown)
			require.Equal(t, bottom, v.YOffset,
				"down intent must return to the newest host-history row")
		})
	}
}

func TestHostHistoryScrollControllerPreservesIntentAcrossPendingResize(t *testing.T) {
	history := lipgloss.JoinVertical(lipgloss.Left, numberedScrollHistory(100), scrollFooter())
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	gen := controller.FillGeneration()

	// The real TUI can resize while capture is in flight. The eventual offset is
	// computed from the new geometry, while both pre/post-resize intents survive.
	controller.Resize(&v, 120, 40)
	controller.Scroll(&v, scrollOneLineUp)
	require.True(t, controller.CompleteFill(gen, &v, history))
	require.Equal(t, 59, v.YOffset, "120x40 bottom 61 minus two queued up intents")
}

func TestHostHistoryScrollControllerReplaysQueuedIntentInOrder(t *testing.T) {
	history := lipgloss.JoinVertical(lipgloss.Left, numberedScrollHistory(100), scrollFooter())
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	// A down request at the newest row clamps there; the subsequent up request
	// must still move one row. Reducing the queue to a net displacement would
	// incorrectly cancel the two and lose their ordering semantics.
	controller.Scroll(&v, scrollOneLineDown)
	controller.Scroll(&v, scrollOneLineUp)
	require.True(t, controller.CompleteFill(controller.FillGeneration(), &v, history))
	require.Equal(t, 76, v.YOffset)
}

func TestHostHistoryScrollControllerPreservesDistanceAcrossReadyResize(t *testing.T) {
	history := lipgloss.JoinVertical(lipgloss.Left, numberedScrollHistory(100), scrollFooter())
	v := viewport.New(80, 24)
	controller := newHostHistoryScrollController()

	controller.Scroll(&v, scrollOneLineUp)
	require.True(t, controller.CompleteFill(controller.FillGeneration(), &v, history))
	require.Equal(t, 76, v.YOffset, "80x24 bottom 77 minus one")

	controller.Resize(&v, 120, 40)
	require.Equal(t, 60, v.YOffset, "120x40 bottom 61 minus the same one-row distance")
	controller.Resize(&v, 80, 24)
	require.Equal(t, 76, v.YOffset, "shrinking must preserve distance from bottom too")
}
