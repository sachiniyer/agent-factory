package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// automationsRailRowDrawn reports what an item row looks like in the rendered
// section: the enabled glyph is the first content of every drawn row, and the
// 1-line compact summary never contains it.
func automationsRailRowDrawn(tasks int, lay layout.Layout) bool {
	a := newTestAutomations(nSimpleTasks(tasks))
	a.SetRect(lay.Automations)
	a.SetCompact(lay.AutomationsCompact)
	return strings.Contains(a.View(), "[✓]")
}

// TestAutomationsRailFocusOnlyWhenARowDraws is #6317: keyboard focus must
// never rest on a rail item that is not drawn. The section renders its 1-line
// compact summary whenever it has no room for an item row — a summary-only
// rail has no row for the cursor to mark, and Enter there would act on an
// item the user cannot see. The minimal-mode cutoff already resolves this at
// 40x10 by hiding the section outright; the compact band between it and the
// first height a row fits left the rail focusable while drawing only its
// header (the reported 60x20 state).
//
// The sweep covers the whole band, not one size: heights from the hard
// minimum up through the first height a row fits, at widths straddling the
// minimal-mode (<60 cols / <15 rows) and compact-mode (<80 cols / <20 rows)
// cuts, with and without a Projects section squeezing the rail. At every size
// the focus-ring admission — the same predicate app wires into SetHidden —
// must agree with what the pane actually drew.
func TestAutomationsRailFocusOnlyWhenARowDraws(t *testing.T) {
	widths := []int{40, 59, 60, 61, 79, 80, 81, 120}
	for _, projects := range []int{0, 2} {
		for h := layout.HardMinHeight; h <= layout.AutomationsFullMinHeight+8; h++ {
			for _, w := range widths {
				lay := layout.Grid{Automations: 3, Projects: projects}.Solve(w, h)
				drewRow := automationsRailRowDrawn(3, lay)

				// The app's admission, verbatim: the ring hides the region
				// exactly while the rail cannot draw an item row.
				ring := layout.NewRing(layout.RegionTree, layout.RegionAutomations)
				ring.SetHidden(layout.RegionAutomations, !lay.AutomationsCanDrawRows())
				focused := ring.Focus(layout.RegionAutomations)

				require.Equalf(t, drewRow, focused,
					"%dx%d projects=%d: focus on the automations rail must exist "+
						"exactly when it draws an item row (drew=%v focused=%v)",
					w, h, projects, drewRow, focused)
				if !focused {
					assert.Equalf(t, layout.RegionTree, ring.Active(),
						"%dx%d: a refused rail leaves focus on the tree, the 40x10 fallback",
						w, h)
				}
			}
		}
	}
}

// TestAutomationsRailCompactSizesRejectFocus pins the reported sizes from
// #6317 as named cases: 60x20 (compact summary — the bug), 40x10 (minimal —
// the existing fallback), and 80x20 (the first height a row fits at full
// width — focus must remain).
func TestAutomationsRailCompactSizesRejectFocus(t *testing.T) {
	for _, tc := range []struct {
		w, h     int
		focused  bool
		drewRows bool
	}{
		{40, 10, false, false},
		{60, 20, false, false},
		{80, 20, true, true},
	} {
		lay := layout.Grid{Automations: 1}.Solve(tc.w, tc.h)
		ring := layout.NewRing(layout.RegionTree, layout.RegionAutomations)
		ring.SetHidden(layout.RegionAutomations, !lay.AutomationsCanDrawRows())
		require.Equalf(t, tc.focused, ring.Focus(layout.RegionAutomations),
			"%dx%d", tc.w, tc.h)
		require.Equalf(t, tc.drewRows, automationsRailRowDrawn(1, lay), "%dx%d", tc.w, tc.h)
	}
}
