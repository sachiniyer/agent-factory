package layout_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// ladderLevel orders the degradation ladder for monotonicity checks: higher
// means more degraded. Levels follow RFC §2.6: full → panes collapsed →
// compact automations → minimal → fallback.
func ladderLevel(l layout.Layout) int {
	switch {
	case l.Fallback:
		return 4
	case !l.AutomationsVisible:
		return 3
	case l.AutomationsCompact:
		return 2
	case l.MaxPanes < 2:
		return 1
	default:
		return 0
	}
}

// TestGridSolveTilesExactly sweeps the full supported size range for several
// requested pane counts and asserts the visible regions exactly tile the
// terminal: no overlap, no gaps, no negative dimensions, nothing outside the
// screen.
func TestGridSolveTilesExactly(t *testing.T) {
	for _, panes := range []int{0, 1, 2, 3} {
		grid := layout.Grid{Panes: panes}
		for w := layout.HardMinWidth; w <= 220; w++ {
			for h := layout.HardMinHeight; h <= 72; h++ {
				l := grid.Solve(w, h)
				require.False(t, l.Fallback, "unexpected fallback at %dx%d", w, h)

				screen := layout.Rect{X: 0, Y: 0, W: w, H: h}
				visible := l.VisibleRegions()
				parts := make([]layout.Rect, 0, len(visible))
				for id, r := range visible {
					require.False(t, r.Empty(), "visible region %s is empty at %dx%d panes=%d", id, w, h, panes)
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
			l := layout.Grid{Panes: 2}.Solve(tt.w, tt.h)
			assert.Equal(t, tt.fallback, l.Fallback)
			if tt.fallback {
				assert.Empty(t, l.VisibleRegions(), "fallback layout must expose no regions")
				assert.Zero(t, l.PaneCount())
				assert.False(t, l.AutomationsVisible)
			}
		})
	}
}

// TestGridSolveMultiPaneThreshold pins the §2.6 multi-pane gate: at
// MultiPaneMinWidth two panes are honored, dividing the workspace evenly
// around a 1-col divider; one column below, the workspace collapses to a
// single pane (the caller retains the hidden panes' bindings).
func TestGridSolveMultiPaneThreshold(t *testing.T) {
	at := layout.Grid{Panes: 2}.Solve(layout.MultiPaneMinWidth, 40)
	require.Equal(t, 2, at.PaneCount(), "two panes honored at %d cols", layout.MultiPaneMinWidth)
	require.Len(t, at.Dividers, 1)
	assert.Equal(t, 1, at.Dividers[0].W, "divider is exactly one column")
	assert.InDelta(t, at.Panes[0].W, at.Panes[1].W, 1, "panes divide the workspace evenly")
	assert.Equal(t, at.Width-at.Tree.W, at.Panes[0].W+at.Dividers[0].W+at.Panes[1].W,
		"panes plus divider fill the workspace")

	below := layout.Grid{Panes: 2}.Solve(layout.MultiPaneMinWidth-1, 40)
	require.Equal(t, 1, below.PaneCount(), "panes collapse to one below %d cols", layout.MultiPaneMinWidth)
	assert.Equal(t, 1, below.MaxPanes)
	assert.Empty(t, below.Dividers)
	assert.Equal(t, below.Width-below.Tree.W, below.Panes[0].W, "the pane takes the whole workspace")

	single := layout.Grid{Panes: 1}.Solve(200, 40)
	assert.Equal(t, 1, single.PaneCount(), "no extra panes unless requested")

	none := layout.Grid{}.Solve(200, 40)
	assert.Zero(t, none.PaneCount(), "no panes requested → bare workspace")
	assert.False(t, none.Workspace.Empty(), "the workspace rect survives for the empty state")
}

// TestGridSolveMaxPanesFitting pins the pane-count fitting math (#1088):
// every laid-out pane is at least PaneMinWidth wide whenever more than one
// is shown, MaxPanes grows with the workspace, and requests beyond MaxPanes
// are clamped rather than squeezed.
func TestGridSolveMaxPanesFitting(t *testing.T) {
	for w := layout.MultiPaneMinWidth; w <= 400; w += 7 {
		for _, req := range []int{1, 2, 3, 5, 9} {
			l := layout.Grid{Panes: req}.Solve(w, 40)
			require.False(t, l.Fallback)
			require.GreaterOrEqual(t, l.MaxPanes, 1)
			want := req
			if want > l.MaxPanes {
				want = l.MaxPanes
			}
			require.Equal(t, want, l.PaneCount(), "w=%d req=%d", w, req)
			if l.PaneCount() > 1 {
				for i, r := range l.Panes {
					require.GreaterOrEqual(t, r.W, layout.PaneMinWidth,
						"pane %d narrower than the minimum at w=%d req=%d", i, w, req)
				}
			}
		}
	}

	// Growing the terminal never shrinks capacity.
	prev := 0
	for w := layout.HardMinWidth; w <= 400; w++ {
		l := layout.Grid{Panes: 9}.Solve(w, 40)
		require.GreaterOrEqual(t, l.MaxPanes, prev, "MaxPanes shrank growing to %d cols", w)
		prev = l.MaxPanes
	}
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

// TestGridSolveAutomationsGrowsToContent pins #1126: with vertical room the
// automations section grows to show every automation (title + one row each +
// one reserved expansion line + one reserved bottom-margin row, #1560)
// instead of the old fixed 3-row cap, and only collapses once the tree +
// automations can't both fit — at which point the half-rail cap keeps the
// tree the priority. The rail still tiles exactly.
func TestGridSolveAutomationsGrowsToContent(t *testing.T) {
	// Zero automations keeps the AutomationsRows floor (a recognizable strip).
	none := layout.Grid{Automations: 0}.Solve(120, 60)
	assert.Equal(t, layout.AutomationsRows, none.Automations.H, "no automations keeps the floor")

	// A handful of automations on a tall rail: the section is exactly title +
	// one row per automation + the reserved expansion line + the reserved
	// bottom-margin row (#1560), and the tree gets the rest of the rail (not the
	// reverse).
	tall := layout.Grid{Automations: 4}.Solve(120, 60)
	require.False(t, tall.AutomationsCompact, "a tall rail is not compact")
	assert.Equal(t, 2+1+4, tall.Automations.H, "the section grows to fit all automations plus the bottom margin")
	assert.Greater(t, tall.Tree.H, tall.Automations.H, "the tree keeps the larger share")

	// More automations than half the rail: the section collapses to (at most)
	// half so the tree keeps at least the other half, and the pane scrolls the
	// overflow. The section never exceeds half the rail-minus-rule.
	many := layout.Grid{Automations: 40}.Solve(100, 30)
	require.False(t, many.AutomationsCompact)
	railH := many.Tree.H + many.RailRule.H + many.Automations.H
	assert.LessOrEqual(t, many.Automations.H, railH/2,
		"crowded automations never take more than half the rail")
	assert.GreaterOrEqual(t, many.Tree.H, many.Automations.H, "the tree keeps at least half")

	// Growth must still tile the screen exactly at every size and count.
	for _, count := range []int{0, 1, 3, 8, 50} {
		grid := layout.Grid{Panes: 1, Automations: count}
		for w := layout.HardMinWidth; w <= 200; w += 7 {
			for h := layout.HardMinHeight; h <= 70; h += 3 {
				l := grid.Solve(w, h)
				require.False(t, l.Fallback)
				screen := layout.Rect{X: 0, Y: 0, W: w, H: h}
				visible := l.VisibleRegions()
				parts := make([]layout.Rect, 0, len(visible))
				for id, r := range visible {
					require.False(t, r.Empty(),
						"region %s empty at %dx%d automations=%d", id, w, h, count)
					parts = append(parts, r)
				}
				requireTiles(t, screen, parts)
			}
		}
	}
}

// TestGridSolveChromeMonotonicWithAutomations re-runs the chrome-shrink
// monotonicity contract with a populated automations section, so the #1126
// dynamic sizing can't regress the "chrome only gives way on shrink" invariant.
func TestGridSolveChromeMonotonicWithAutomations(t *testing.T) {
	grid := layout.Grid{Panes: 2, Automations: 10}
	prev := grid.Solve(200, 72)
	for h := 71; h >= 1; h-- {
		cur := grid.Solve(200, h)
		require.LessOrEqual(t, cur.Automations.H, prev.Automations.H,
			"automations grew shrinking height to %d", h)
		prev = cur
	}
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
			l := layout.Grid{Panes: 2}.Solve(tt.w, tt.h)
			require.False(t, l.Fallback)
			assert.False(t, l.AutomationsVisible, "minimal mode hides the automations strip")
			assert.True(t, l.Automations.Empty())
			assert.Equal(t, 1, l.MaxPanes, "minimal mode never honors more than one pane")
			assert.Equal(t, 1, l.PaneCount())
			assert.False(t, l.Tree.Empty(), "tree survives minimal mode")
			assert.False(t, l.Panes[0].Empty(), "one pane survives minimal mode")
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
		{60, layout.TreeMinWidth},  // 25% = 15 → clamped up to 22
		{88, layout.TreeMinWidth},  // 25% = 22 → exactly the minimum
		{100, 25},                  // 25% in range
		{120, 30},                  // 25% in range
		{135, 33},                  // 25% = 33.75, integer math
		{144, layout.TreeMaxWidth}, // 25% = 36 → exactly the maximum
		{150, layout.TreeMaxWidth}, // 25% = 37.5 → clamped down to 36
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
		l := layout.Grid{Panes: 2}.Solve(size[0], size[1])
		require.False(t, l.Fallback)
		assert.Equal(t, layout.StatusBarRows, l.StatusBar.H, "at %v", size)
		assert.Equal(t, size[0], l.StatusBar.W, "status bar spans the full width at %v", size)
		assert.Equal(t, size[1], l.StatusBar.Bottom(), "status bar sits at the bottom at %v", size)
	}
}

// TestGridSolveBannerReservesTopRow pins the delivery-failure alarm banner
// reservation (#1238): with Grid.Banner set, a full-width AlarmBarRows band is
// cut from the very top, every other region shifts below it, and the banner
// plus the visible regions still tile the screen exactly. Without Banner the
// rect is empty and nothing is reserved.
func TestGridSolveBannerReservesTopRow(t *testing.T) {
	for _, size := range [][2]int{{40, 10}, {59, 14}, {80, 24}, {200, 60}} {
		w, h := size[0], size[1]

		// No banner requested → no row reserved.
		off := layout.Grid{Panes: 2}.Solve(w, h)
		require.True(t, off.Banner.Empty(), "no banner row without Grid.Banner at %v", size)

		on := layout.Grid{Panes: 2, Banner: true}.Solve(w, h)
		require.False(t, on.Fallback)
		require.Equal(t, layout.Rect{X: 0, Y: 0, W: w, H: layout.AlarmBarRows}, on.Banner,
			"banner is a full-width top band at %v", size)

		// Every other region sits strictly below the banner.
		visible := on.VisibleRegions()
		parts := make([]layout.Rect, 0, len(visible)+1)
		for id, r := range visible {
			assert.GreaterOrEqual(t, r.Y, layout.AlarmBarRows,
				"region %s must sit below the banner at %v", id, size)
			parts = append(parts, r)
		}
		// The banner is passive (not in VisibleRegions); include it to prove the
		// banner + regions still tile the whole screen with no gap or overlap.
		parts = append(parts, on.Banner)
		requireTiles(t, layout.Rect{X: 0, Y: 0, W: w, H: h}, parts)
	}
}

// TestGridSolveBannerSurvivesMinimalMode proves an active outage alarm is
// reserved even in minimal mode — a cramped terminal must not hide it (#1238).
func TestGridSolveBannerSurvivesMinimalMode(t *testing.T) {
	// Just inside minimal (below MinimalWidth) but above the hard minimum.
	w, h := layout.MinimalWidth-1, layout.MinimalHeight
	l := layout.Grid{Panes: 2, Banner: true}.Solve(w, h)
	require.False(t, l.Fallback)
	require.False(t, l.AutomationsVisible, "sanity: this size is minimal mode")
	require.Equal(t, layout.AlarmBarRows, l.Banner.H, "the banner is reserved in minimal mode too")
	require.Equal(t, 0, l.Banner.Y)
}

// TestGridSolveLadderMonotonic shrinks each axis one cell at a time and
// asserts the degradation level never decreases: shrinking can never
// re-enable a richer mode.
func TestGridSolveLadderMonotonic(t *testing.T) {
	grid := layout.Grid{Panes: 2}

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
// degradation — the surviving panes absorb a hidden one's width, the
// workspace reclaims the automations rows when the strip compacts.)
func TestGridSolveChromeNeverGrowsOnShrink(t *testing.T) {
	grid := layout.Grid{Panes: 2}

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
// surviving panes absorb a hidden one, the workspace reclaims automations
// rows — see TestGridSolveChromeNeverGrowsOnShrink for the global invariant.)
func TestGridSolveRegionsMonotonicWithinLevel(t *testing.T) {
	grid := layout.Grid{Panes: 2}

	sameState := func(a, b layout.Layout) bool {
		return a.Fallback == b.Fallback &&
			a.PaneCount() == b.PaneCount() &&
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

func TestGridVisibleRegionsMatchLayout(t *testing.T) {
	l := layout.Grid{Panes: 2}.Solve(160, 48)
	require.Equal(t, 2, l.PaneCount())
	require.True(t, l.AutomationsVisible)
	regions := l.VisibleRegions()
	assert.Len(t, regions, 7)
	assert.Equal(t, l.Tree, regions[layout.RegionTree])
	assert.Equal(t, l.Panes[0], regions[layout.PaneRegion(0)])
	assert.Equal(t, l.Panes[1], regions[layout.PaneRegion(1)])
	assert.Equal(t, l.Dividers[0], regions[layout.DividerRegion(0)])
	assert.Equal(t, l.RailRule, regions[layout.RegionRailRule])
	assert.Equal(t, l.Automations, regions[layout.RegionAutomations])
	assert.Equal(t, l.StatusBar, regions[layout.RegionStatusBar])
	assert.NotContains(t, regions, layout.RegionWorkspace,
		"the bare workspace region only exists with no panes open")

	single := layout.Grid{Panes: 1}.Solve(160, 48)
	regions = single.VisibleRegions()
	assert.Len(t, regions, 5)
	assert.NotContains(t, regions, layout.PaneRegion(1))
	assert.NotContains(t, regions, layout.DividerRegion(0))

	empty := layout.Grid{}.Solve(160, 48)
	regions = empty.VisibleRegions()
	assert.Equal(t, empty.Workspace, regions[layout.RegionWorkspace],
		"no panes open → the workspace itself is the region")

	minimal := layout.Grid{}.Solve(50, 12)
	regions = minimal.VisibleRegions()
	assert.Len(t, regions, 3)
	assert.NotContains(t, regions, layout.RegionAutomations)
	assert.NotContains(t, regions, layout.RegionRailRule)
}

// TestGridSolveAutomationsInRail pins the #1087/#1090 geometry: the
// automations section lives INSIDE the left rail, bottom-aligned against the
// status bar, separated from the tree by a 1-row full-rail-width rule — and
// the workspace panes run the full height above the status bar.
func TestGridSolveAutomationsInRail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grid  layout.Grid
		w, h  int
		panes int
	}{
		{"wide", layout.Grid{Panes: 1}, 160, 48, 1},
		{"canonical-80x24", layout.Grid{Panes: 1}, 80, 24, 1},
		{"compact", layout.Grid{Panes: 1}, 79, 22, 1},
		{"two-pane", layout.Grid{Panes: 2}, 160, 48, 2},
		{"three-pane", layout.Grid{Panes: 3}, 220, 48, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.grid.Solve(tc.w, tc.h)
			require.True(t, l.AutomationsVisible)
			require.Equal(t, tc.panes, l.PaneCount())

			// Rail column: tree, rule, automations share X and W and stack
			// exactly, with automations pinned to the status bar.
			assert.Equal(t, l.Tree.X, l.RailRule.X)
			assert.Equal(t, l.Tree.X, l.Automations.X)
			assert.Equal(t, l.Tree.W, l.RailRule.W, "the rule spans the full rail width")
			assert.Equal(t, l.Tree.W, l.Automations.W, "automations span the full rail width")
			assert.Equal(t, layout.RailRuleRows, l.RailRule.H)
			assert.Equal(t, l.Tree.Bottom(), l.RailRule.Y, "the rule sits directly under the tree")
			assert.Equal(t, l.RailRule.Bottom(), l.Automations.Y, "automations sit directly under the rule")
			assert.Equal(t, l.StatusBar.Y, l.Automations.Bottom(), "automations are bottom-aligned in the rail")

			// Workspace: full height above the status bar (#1090), every
			// pane and divider full-height too.
			assert.Equal(t, tc.h-layout.StatusBarRows, l.Workspace.H)
			for i, r := range l.Panes {
				assert.Equal(t, l.Workspace.H, r.H, "pane %d takes the full workspace height", i)
				assert.Equal(t, l.StatusBar.Y, r.Bottom())
			}
			for i, r := range l.Dividers {
				assert.Equal(t, l.Workspace.H, r.H, "divider %d takes the full workspace height", i)
			}
		})
	}
}
