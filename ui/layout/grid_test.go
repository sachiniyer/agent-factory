package layout_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// ladderLevel orders the degradation ladder for monotonicity checks: higher
// means more degraded. Levels follow RFC §2.6: full → split collapsed →
// compact automations → minimal → fallback.
func ladderLevel(l layout.Layout) int {
	switch {
	case l.Fallback:
		return 4
	case !l.AutomationsVisible:
		return 3
	case l.AutomationsCompact:
		return 2
	case !l.SplitActive:
		return 1
	default:
		return 0
	}
}

// TestGridSolveTilesExactly sweeps the full supported size range, with and
// without a split requested, and asserts the visible regions exactly tile
// the terminal: no overlap, no gaps, no negative dimensions, nothing
// outside the screen.
func TestGridSolveTilesExactly(t *testing.T) {
	for _, split := range []bool{false, true} {
		grid := layout.Grid{Split: split}
		for w := layout.HardMinWidth; w <= 220; w++ {
			for h := layout.HardMinHeight; h <= 72; h++ {
				l := grid.Solve(w, h)
				require.False(t, l.Fallback, "unexpected fallback at %dx%d", w, h)

				screen := layout.Rect{X: 0, Y: 0, W: w, H: h}
				visible := l.VisibleRegions()
				parts := make([]layout.Rect, 0, len(visible))
				for id, r := range visible {
					require.False(t, r.Empty(), "visible region %s is empty at %dx%d split=%v", id, w, h, split)
					parts = append(parts, r)
				}
				requireTiles(t, screen, parts)
			}
		}
	}
}

func TestGridSolveFallbackBelowHardMinimum(t *testing.T) {
	tests := []struct {
		w, h     int
		fallback bool
	}{
		{layout.HardMinWidth, layout.HardMinHeight, false},
		{layout.HardMinWidth - 1, 50, true},
		{200, layout.HardMinHeight - 1, true},
		{layout.HardMinWidth - 1, layout.HardMinHeight - 1, true},
		{0, 0, true},
		{-5, 24, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%dx%d", tt.w, tt.h), func(t *testing.T) {
			l := layout.Grid{Split: true}.Solve(tt.w, tt.h)
			assert.Equal(t, tt.fallback, l.Fallback)
			if tt.fallback {
				assert.Empty(t, l.VisibleRegions(), "fallback layout must expose no regions")
				assert.False(t, l.SplitActive)
				assert.False(t, l.AutomationsVisible)
			}
		})
	}
}

func TestGridSolveSplitThreshold(t *testing.T) {
	at := layout.Grid{Split: true}.Solve(layout.SplitMinWidth, 40)
	require.True(t, at.SplitActive, "split honored at %d cols", layout.SplitMinWidth)
	assert.False(t, at.PaneB.Empty())
	assert.Equal(t, 1, at.Divider.W, "divider is exactly one column")
	assert.InDelta(t, at.PaneA.W, at.PaneB.W, 1, "split divides the workspace evenly")
	assert.Equal(t, at.Width-at.Tree.W, at.PaneA.W+at.Divider.W+at.PaneB.W,
		"panes plus divider fill the workspace")

	below := layout.Grid{Split: true}.Solve(layout.SplitMinWidth-1, 40)
	require.False(t, below.SplitActive, "split collapses below %d cols", layout.SplitMinWidth)
	assert.True(t, below.PaneB.Empty())
	assert.True(t, below.Divider.Empty())
	assert.Equal(t, below.Width-below.Tree.W, below.PaneA.W, "pane A takes the whole workspace")

	unrequested := layout.Grid{}.Solve(200, 40)
	assert.False(t, unrequested.SplitActive, "no split unless requested")
	assert.True(t, unrequested.PaneB.Empty())
}

func TestGridSolveAutomationsThresholds(t *testing.T) {
	full := layout.Grid{}.Solve(layout.AutomationsFullMinWidth, 40)
	require.True(t, full.AutomationsVisible)
	assert.False(t, full.AutomationsCompact)
	assert.Equal(t, layout.AutomationsRows, full.Automations.H)

	narrowCompact := layout.Grid{}.Solve(layout.AutomationsFullMinWidth-1, 40)
	require.True(t, narrowCompact.AutomationsVisible)
	assert.True(t, narrowCompact.AutomationsCompact, "strip compacts below %d cols", layout.AutomationsFullMinWidth)
	assert.Equal(t, layout.AutomationsCompactRows, narrowCompact.Automations.H)

	shortCompact := layout.Grid{}.Solve(120, layout.AutomationsFullMinHeight-1)
	require.True(t, shortCompact.AutomationsVisible)
	assert.True(t, shortCompact.AutomationsCompact, "strip compacts below %d rows", layout.AutomationsFullMinHeight)
	assert.Equal(t, layout.AutomationsCompactRows, shortCompact.Automations.H)
}

func TestGridSolveMinimalMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		w, h int
	}{
		{"narrow", layout.MinimalWidth - 1, 40},
		{"short", 200, layout.MinimalHeight - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := layout.Grid{Split: true}.Solve(tt.w, tt.h)
			require.False(t, l.Fallback)
			assert.False(t, l.AutomationsVisible, "minimal mode hides the automations strip")
			assert.True(t, l.Automations.Empty())
			assert.False(t, l.SplitActive, "minimal mode never honors a split")
			assert.False(t, l.Tree.Empty(), "tree survives minimal mode")
			assert.False(t, l.PaneA.Empty(), "pane A survives minimal mode")
			assert.False(t, l.StatusBar.Empty(), "status bar survives minimal mode")
		})
	}

	above := layout.Grid{}.Solve(layout.MinimalWidth, layout.MinimalHeight)
	assert.True(t, above.AutomationsVisible, "automations return at exactly %dx%d",
		layout.MinimalWidth, layout.MinimalHeight)
}

func TestGridSolveTreeWidthClamp(t *testing.T) {
	tests := []struct {
		w    int
		want int
	}{
		{60, layout.TreeMinWidth},  // 30% = 18 → clamped up to 24
		{80, layout.TreeMinWidth},  // 30% = 24 → exactly the minimum
		{100, 30},                  // 30% in range
		{120, 36},                  // 30% in range
		{146, 43},                  // 30% = 43.8, integer math
		{150, layout.TreeMaxWidth}, // 30% = 45 → clamped down to 44
		{220, layout.TreeMaxWidth},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("w=%d", tt.w), func(t *testing.T) {
			l := layout.Grid{}.Solve(tt.w, 40)
			assert.Equal(t, tt.want, l.Tree.W)
		})
	}
}

func TestGridSolveStatusBarFixedHeight(t *testing.T) {
	for _, size := range [][2]int{{40, 10}, {59, 14}, {80, 24}, {200, 60}} {
		l := layout.Grid{Split: true}.Solve(size[0], size[1])
		require.False(t, l.Fallback)
		assert.Equal(t, layout.StatusBarRows, l.StatusBar.H, "at %v", size)
		assert.Equal(t, size[0], l.StatusBar.W, "status bar spans the full width at %v", size)
		assert.Equal(t, size[1], l.StatusBar.Bottom(), "status bar sits at the bottom at %v", size)
	}
}

// TestGridSolveLadderMonotonic shrinks each axis one cell at a time and
// asserts the degradation level never decreases: shrinking can never
// re-enable a richer mode.
func TestGridSolveLadderMonotonic(t *testing.T) {
	grid := layout.Grid{Split: true}

	prev := -1
	for w := 220; w >= 1; w-- {
		lvl := ladderLevel(grid.Solve(w, 40))
		require.GreaterOrEqual(t, lvl, prev, "ladder regressed shrinking width to %d", w)
		prev = lvl
	}

	prev = -1
	for h := 72; h >= 1; h-- {
		lvl := ladderLevel(grid.Solve(200, h))
		require.GreaterOrEqual(t, lvl, prev, "ladder regressed shrinking height to %d", h)
		prev = lvl
	}
}

// TestGridSolveChromeNeverGrowsOnShrink asserts the global monotonicity
// contract of the ladder: chrome (tree width, automations height, status
// height) only ever gives way as the terminal shrinks — it never grows.
// (Content panes CAN grow across ladder transitions: that is the point of
// degradation — pane A absorbs pane B below SplitMinWidth, the workspace
// reclaims the automations rows when the strip compacts.)
func TestGridSolveChromeNeverGrowsOnShrink(t *testing.T) {
	grid := layout.Grid{Split: true}

	check := func(t *testing.T, cur, prev layout.Layout) {
		t.Helper()
		require.LessOrEqual(t, cur.Tree.W, prev.Tree.W,
			"tree grew shrinking %dx%d→%dx%d", prev.Width, prev.Height, cur.Width, cur.Height)
		require.LessOrEqual(t, cur.Automations.H, prev.Automations.H,
			"automations grew shrinking %dx%d→%dx%d", prev.Width, prev.Height, cur.Width, cur.Height)
		require.LessOrEqual(t, cur.StatusBar.H, prev.StatusBar.H,
			"status bar grew shrinking %dx%d→%dx%d", prev.Width, prev.Height, cur.Width, cur.Height)
	}

	prev := grid.Solve(220, 40)
	for w := 219; w >= 1; w-- {
		cur := grid.Solve(w, 40)
		check(t, cur, prev)
		prev = cur
	}

	prev = grid.Solve(200, 72)
	for h := 71; h >= 1; h-- {
		cur := grid.Solve(200, h)
		check(t, cur, prev)
		prev = cur
	}
}

// TestGridSolveRegionsMonotonicWithinLevel asserts that between two
// adjacent sizes in the same degradation state, shrinking never grows any
// region dimension. (Across ladder transitions content legitimately grows —
// pane A absorbs pane B, the workspace reclaims automations rows — see
// TestGridSolveChromeNeverGrowsOnShrink for the global invariant.)
func TestGridSolveRegionsMonotonicWithinLevel(t *testing.T) {
	grid := layout.Grid{Split: true}

	sameState := func(a, b layout.Layout) bool {
		return a.Fallback == b.Fallback &&
			a.SplitActive == b.SplitActive &&
			a.AutomationsVisible == b.AutomationsVisible &&
			a.AutomationsCompact == b.AutomationsCompact
	}
	check := func(t *testing.T, cur, prev layout.Layout) {
		t.Helper()
		if !sameState(cur, prev) || cur.Fallback {
			return
		}
		prevRegions := prev.VisibleRegions()
		for id, r := range cur.VisibleRegions() {
			pr, ok := prevRegions[id]
			require.True(t, ok, "region %s appeared on shrink at %dx%d", id, cur.Width, cur.Height)
			require.LessOrEqual(t, r.W, pr.W, "region %s width grew shrinking to %dx%d", id, cur.Width, cur.Height)
			require.LessOrEqual(t, r.H, pr.H, "region %s height grew shrinking to %dx%d", id, cur.Width, cur.Height)
		}
	}

	for h := 10; h <= 72; h += 7 {
		prev := grid.Solve(220, h)
		for w := 219; w >= layout.HardMinWidth; w-- {
			cur := grid.Solve(w, h)
			check(t, cur, prev)
			prev = cur
		}
	}
	for w := 40; w <= 220; w += 9 {
		prev := grid.Solve(w, 72)
		for h := 71; h >= layout.HardMinHeight; h-- {
			cur := grid.Solve(w, h)
			check(t, cur, prev)
			prev = cur
		}
	}
}

func TestGridVisibleRegionsMatchFlags(t *testing.T) {
	l := layout.Grid{Split: true}.Solve(160, 48)
	require.True(t, l.SplitActive)
	require.True(t, l.AutomationsVisible)
	regions := l.VisibleRegions()
	assert.Len(t, regions, 6)
	assert.Equal(t, l.Tree, regions[layout.RegionTree])
	assert.Equal(t, l.PaneA, regions[layout.RegionPaneA])
	assert.Equal(t, l.PaneB, regions[layout.RegionPaneB])
	assert.Equal(t, l.Divider, regions[layout.RegionDivider])
	assert.Equal(t, l.Automations, regions[layout.RegionAutomations])
	assert.Equal(t, l.StatusBar, regions[layout.RegionStatusBar])

	single := layout.Grid{}.Solve(160, 48)
	regions = single.VisibleRegions()
	assert.Len(t, regions, 4)
	assert.NotContains(t, regions, layout.RegionPaneB)
	assert.NotContains(t, regions, layout.RegionDivider)

	minimal := layout.Grid{}.Solve(50, 12)
	regions = minimal.VisibleRegions()
	assert.Len(t, regions, 3)
	assert.NotContains(t, regions, layout.RegionAutomations)
}

// TestGridSolveAutomationsExpandedTilesExactly sweeps the size range with the
// automations strip expanded in place (#1024 PR 4: focusing the strip swaps
// the compact rows for the full task manager) and asserts the regions still
// exactly tile the terminal.
func TestGridSolveAutomationsExpandedTilesExactly(t *testing.T) {
	grid := layout.Grid{AutomationsExpanded: true}
	for w := layout.HardMinWidth; w <= 220; w += 3 {
		for h := layout.HardMinHeight; h <= 72; h++ {
			l := grid.Solve(w, h)
			require.False(t, l.Fallback, "unexpected fallback at %dx%d", w, h)

			screen := layout.Rect{X: 0, Y: 0, W: w, H: h}
			visible := l.VisibleRegions()
			parts := make([]layout.Rect, 0, len(visible))
			for id, r := range visible {
				require.False(t, r.Empty(), "visible region %s is empty at %dx%d", id, w, h)
				parts = append(parts, r)
			}
			requireTiles(t, screen, parts)
		}
	}
}

// TestGridSolveAutomationsExpandedAllocation pins the expanded strip's
// contract: honored whenever the strip is visible, never compact (an editor
// cannot run in one line), roughly half the rows above the status bar, and
// the workspace keeps at least as much as the strip.
func TestGridSolveAutomationsExpandedAllocation(t *testing.T) {
	grid := layout.Grid{AutomationsExpanded: true}

	l := grid.Solve(100, 30)
	require.True(t, l.AutomationsVisible)
	assert.True(t, l.AutomationsExpanded, "expansion honored outside minimal mode")
	assert.False(t, l.AutomationsCompact, "expansion overrides the compact degradation")
	assert.Equal(t, (30-layout.StatusBarRows)/2, l.Automations.H,
		"expanded strip takes half the rows above the status bar")
	assert.GreaterOrEqual(t, l.PaneA.H, l.Automations.H,
		"the workspace keeps at least as many rows as the strip")

	// Below the compact threshold the expansion still wins (the manager needs
	// the rows).
	tight := grid.Solve(70, 24)
	require.True(t, tight.AutomationsVisible)
	assert.True(t, tight.AutomationsExpanded)
	assert.False(t, tight.AutomationsCompact)

	// Minimal mode hides the strip entirely; the request is moot.
	minimal := grid.Solve(59, 14)
	assert.False(t, minimal.AutomationsVisible)
	assert.False(t, minimal.AutomationsExpanded)
}
