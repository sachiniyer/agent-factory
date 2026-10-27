package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// TestAdjacentPaneSeamRendersGapNotTripleBorder is the #5279 pin: adjacent
// panes each draw their own frame, so a drawn │ divider stacked a third bar
// between them and every seam read "│││" (corners "┐│┌", "┘│└"). The divider
// column is a blank gap, leaving "│ │" — the two pane frames still mark the
// boundary and the column count (and therefore pane width math and click
// targets) does not move.
func TestAdjacentPaneSeamRendersGapNotTripleBorder(t *testing.T) {
	for _, panes := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("%d panes", panes), func(t *testing.T) {
			lay := layout.Grid{Panes: panes}.Solve(200, 40)
			require.Len(t, lay.Panes, panes)
			require.Len(t, lay.Dividers, panes-1)

			cols := make([]string, 0, 2*panes-1)
			for i, r := range lay.Panes {
				if i > 0 {
					gap := PaneGap(lay.Dividers[i-1])
					requireExactRect(t, gap, lay.Dividers[i-1], "pane gap")
					cols = append(cols, gap)
				}
				w := newTestTabbedWindow()
				w.SetRect(r)
				cols = append(cols, w.View())
			}

			top := layout.JoinHorizontal(cols...)
			requireExactRect(t, top, lay.Workspace, "tiled workspace")

			for i, line := range strings.Split(top, "\n") {
				require.NotContainsf(t, line, "│││",
					"row %d: a drawn divider stacked a third border between two pane frames (#5279)", i)
				require.NotContainsf(t, line, "┐│┌",
					"row %d: seam corners must not triple either (#5279)", i)
				require.NotContainsf(t, line, "┘│└",
					"row %d: seam corners must not triple either (#5279)", i)
			}
			require.Contains(t, top, "│ │",
				"the seam reads as the two pane frames across a one-cell gap")
			require.Contains(t, top, "┐ ┌",
				"top corners keep the same one-cell gap")
		})
	}
}
