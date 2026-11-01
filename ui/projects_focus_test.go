package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// nSimpleProjects builds n distinct projects for the rail row-drawn sweep. The
// names are chosen so the first project's name appears in a drawn row but never
// in the 1-line compact summary (the summary is "Projects: N", body-only).
func nSimpleProjects(n int) []SidebarProject {
	out := make([]SidebarProject, 0, n)
	for i := 0; i < n; i++ {
		root := fmt.Sprintf("/repos/project-%d", i)
		out = append(out, SidebarProject{
			Name:         fmt.Sprintf("project-%d", i),
			Root:         root,
			SessionCount: i,
		})
	}
	return out
}

// projectsRailRowDrawn reports what an item row looks like in the rendered
// section: the first project's name is content of the first drawn row, and the
// 1-line compact summary never contains it (the summary is the header count
// only — "Projects: N"). This mirrors automationsRailRowDrawn's use of the
// enabled glyph as the row-only marker.
func projectsRailRowDrawn(rows int, lay layout.Layout) bool {
	p := newTestProjects(nSimpleProjects(rows))
	p.SetRect(lay.Projects)
	p.SetCompact(lay.ProjectsCompact)
	return strings.Contains(p.View(), "project-0")
}

// TestProjectsRailFocusOnlyWhenARowDraws mirrors TestAutomationsRailFocusOnlyWhenARowDraws
// for the Projects section: keyboard focus must never rest on a rail item that
// is not drawn. The section renders its 1-line compact summary whenever it has
// no room for an item row — a summary-only rail has no row for the cursor to
// mark, and Enter there would act on an item the user cannot see (the #6317
// contract, which the sibling Automations region already enforces). The
// minimal-mode cutoff resolves this at the smallest sizes by hiding the section
// outright; the compact band between it and the first height a row fits left the
// rail focusable while drawing only its header — the reported 70x30 state.
//
// The sweep covers the whole band, not one size: heights from the hard minimum
// up through the first height a row fits, at widths straddling the minimal-mode
// (<60 cols / <15 rows) and compact-mode (<80 cols / <20 rows) cuts, with and
// without an Automations section squeezing the rail. At every size the
// focus-ring admission — the same predicate app wires into SetHidden — must
// agree with what the pane actually drew.
func TestProjectsRailFocusOnlyWhenARowDraws(t *testing.T) {
	widths := []int{40, 59, 60, 61, 70, 79, 80, 81, 120}
	for _, automations := range []int{0, 3} {
		for h := layout.HardMinHeight; h <= layout.AutomationsFullMinHeight+8; h++ {
			for _, w := range widths {
				lay := layout.Grid{Automations: automations, Projects: 2}.Solve(w, h)
				drewRow := projectsRailRowDrawn(2, lay)

				// The app's admission, verbatim: the ring hides the region
				// exactly while the rail cannot draw an item row.
				ring := layout.NewRing(layout.RegionTree, layout.RegionProjects)
				ring.SetHidden(layout.RegionProjects, !lay.ProjectsCanDrawRows())
				focused := ring.Focus(layout.RegionProjects)

				require.Equalf(t, drewRow, focused,
					"%dx%d automations=%d: focus on the projects rail must exist "+
						"exactly when it draws an item row (drew=%v focused=%v)",
					w, h, automations, drewRow, focused)
				if !focused {
					assert.Equalf(t, layout.RegionTree, ring.Active(),
						"%dx%d: a refused rail leaves focus on the tree, the 40x10 fallback",
						w, h)
				}
			}
		}
	}
}

// TestProjectsRailCompactSizesRejectFocus pins the reported sizes from the bug
// report as named cases: 70x30 (compact summary — the bug), 60x20 (compact at
// the minimal-mode width boundary), 40x10 (minimal — the existing fallback),
// and 80x20 (the first height a row fits at full width — focus must remain).
func TestProjectsRailCompactSizesRejectFocus(t *testing.T) {
	for _, tc := range []struct {
		w, h     int
		focused  bool
		drewRows bool
	}{
		{40, 10, false, false}, // minimal: section dropped outright
		{60, 20, false, false}, // compact band: summary only
		{70, 30, false, false}, // compact band (the reported state): summary only
		{80, 20, true, true},   // first height a row fits at full width
	} {
		lay := layout.Grid{Automations: 3, Projects: 2}.Solve(tc.w, tc.h)
		ring := layout.NewRing(layout.RegionTree, layout.RegionProjects)
		ring.SetHidden(layout.RegionProjects, !lay.ProjectsCanDrawRows())
		require.Equalf(t, tc.focused, ring.Focus(layout.RegionProjects),
			"%dx%d", tc.w, tc.h)
		require.Equalf(t, tc.drewRows, projectsRailRowDrawn(2, lay), "%dx%d", tc.w, tc.h)
	}
}

// TestProjectsRailInvisibleRegionUnfocusable pins the minimal-mode / empty-rail
// path: when the section is dropped outright (ProjectsVisible=false), the
// region is unfocusable even though ProjectsCanDrawRows's H check would already
// refuse it — the visible predicate is the first gate.
func TestProjectsRailInvisibleRegionUnfocusable(t *testing.T) {
	// 59x15: minimal mode drops the Projects section entirely.
	lay := layout.Grid{Automations: 3, Projects: 2}.Solve(59, 15)
	require.False(t, lay.ProjectsVisible, "minimal mode hides the Projects section")
	require.False(t, lay.ProjectsCanDrawRows(),
		"an invisible region can never draw rows")

	ring := layout.NewRing(layout.RegionTree, layout.RegionProjects)
	ring.SetHidden(layout.RegionProjects, !lay.ProjectsCanDrawRows())
	require.False(t, ring.Focus(layout.RegionProjects),
		"an invisible region must not receive focus")
	require.Equal(t, layout.RegionTree, ring.Active(),
		"a refused rail leaves focus on the tree")

	// A single project reserves no rail rows, so the section is invisible too.
	lay = layout.Grid{Automations: 0, Projects: 1}.Solve(120, 40)
	require.False(t, lay.ProjectsVisible, "a single project reserves no rail rows")
	require.False(t, lay.ProjectsCanDrawRows())
}

// TestProjectsCompactCursorIsInvisibleWithoutFocus guards the root cause from
// the user's view: while the section is in the compact band, focusing it AND
// moving the cursor must leave the rendered View byte-identical (no row is
// drawn for the cursor to mark), so the fix's focus admission — not a key
// guard — is what prevents Enter from acting on the invisible selection. This
// pins the #6317 failure mode the focus-ring fix closes off.
func TestProjectsCompactCursorIsInvisibleWithoutFocus(t *testing.T) {
	// The reported compact-band state: 70x30 with two projects.
	lay := layout.Grid{Automations: 3, Projects: 2}.Solve(70, 30)
	require.True(t, lay.ProjectsVisible)
	require.True(t, lay.ProjectsCompact)

	p := newTestProjects(nSimpleProjects(2))
	p.SetRect(lay.Projects)
	p.SetCompact(lay.ProjectsCompact)
	p.Focus()

	before := p.View()
	require.NotEmpty(t, before)
	require.NotContains(t, before, "project-0",
		"the compact summary draws no project row for a cursor to mark")

	// The pane's own key guard only checks focus — in compact mode the cursor
	// still moves invisibly. This is exactly why the focus-ring admission is
	// the fix: the cursor moves while nothing is drawn.
	_, consumed := p.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	require.True(t, consumed, "a focused compact pane still consumes cursor keys")
	require.Equal(t, before, p.View(),
		"the compact View is byte-identical across an invisible cursor move")
}
