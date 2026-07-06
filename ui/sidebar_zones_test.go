package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// ----------------------------------------------------------------------------
// Mouse zone registration (#1024 R4): the sidebar registers a hit rect per
// interactive row while rendering, and the rects must line up with the actual
// rendered output — the tests derive expectations from the registry and check
// them against the frame, never against hardcoded coordinates.
// ----------------------------------------------------------------------------

// plainLines strips ANSI and splits a rendered frame into lines.
func plainLines(out string) []string {
	return strings.Split(xansi.Strip(out), "\n")
}

// runeAtCell returns the rune occupying terminal cell col of a plain line.
func runeAtCell(line string, col int) rune {
	w := 0
	for _, r := range line {
		if w == col {
			return r
		}
		w += runewidth.RuneWidth(r)
	}
	return 0
}

// lineAt maps an absolute zone row back into the pane-local rendered line.
func lineAt(t *testing.T, lines []string, rect layout.Rect, y int) string {
	t.Helper()
	local := y - rect.Y
	require.GreaterOrEqual(t, local, 0)
	require.Less(t, local, len(lines), "zone row must exist in the rendered output")
	return lines[local]
}

func TestSidebarRegistersRowZones(t *testing.T) {
	s := newTreeSidebar(t, 3)
	reg := zones.NewRegistry()
	s.SetZoneRegistry(reg)
	rect := layout.Rect{X: 7, Y: 3, W: 40, H: 24}
	s.SetRect(rect)
	s.SetSelectedInstance(0)

	reg.Reset()
	out := s.String()
	lines := plainLines(out)

	// The pane background is the whole rect (any stray click focuses the tree).
	bg, ok := reg.Find(zones.TreeBG)
	require.True(t, ok)
	assert.Equal(t, rect, bg)

	// The section header row renders the section title on its zone's row.
	header, ok := reg.Find(zones.TreeHeader)
	require.True(t, ok, "header zone must be registered")
	assert.Contains(t, lineAt(t, lines, rect, header.Y), "Instances",
		"the header zone must sit on the rendered header row")

	// Every instance row has a zone whose block contains its title.
	for _, title := range []string{"t-00", "t-01", "t-02"} {
		r, ok := reg.Find(zones.TreeInstance(title))
		require.True(t, ok, "instance zone for %s; got %v", title, reg.IDs())
		assert.Equal(t, rect.X, r.X)
		assert.Equal(t, rect.W, r.W)
		block := strings.Join(lines[r.Y-rect.Y:r.Y-rect.Y+r.H], "\n")
		assert.Contains(t, block, title, "the zone block must contain the row it targets")
	}

	// The selected (expanded) instance registers its tab child rows, and each
	// zone's row renders that tab's label.
	for idx, label := range []string{"1 Agent", "2 Terminal"} {
		r, ok := reg.Find(zones.TreeTab("t-00", idx))
		require.True(t, ok, "tab zone for slot %d; got %v", idx, reg.IDs())
		assert.Equal(t, 1, r.H, "tab rows are single-line")
		assert.Contains(t, lineAt(t, lines, rect, r.Y), label)
	}
	// Collapsed instances register no tab zones.
	_, ok = reg.Find(zones.TreeTab("t-01", 0))
	assert.False(t, ok, "collapsed instances must not register tab zones")
}

func TestSidebarArrowZoneMatchesRenderedGlyph(t *testing.T) {
	s := newTreeSidebar(t, 2)
	reg := zones.NewRegistry()
	s.SetZoneRegistry(reg)
	rect := layout.Rect{X: 4, Y: 2, W: 38, H: 22}
	s.SetRect(rect)
	s.SetSelectedInstance(0)

	reg.Reset()
	lines := plainLines(s.String())

	// Expanded selected instance: its arrow zone must sit exactly on the ▾.
	r, ok := reg.Find(zones.TreeArrow("t-00"))
	require.True(t, ok, "arrow zone for the expanded instance")
	assert.Equal(t, '▾', runeAtCell(lineAt(t, lines, rect, r.Y), r.X-rect.X),
		"the arrow zone must cover the rendered ▾ glyph")

	// Collapsed instance: same contract for ▸.
	r2, ok := reg.Find(zones.TreeArrow("t-01"))
	require.True(t, ok, "arrow zone for the collapsed instance")
	assert.Equal(t, '▸', runeAtCell(lineAt(t, lines, rect, r2.Y), r2.X-rect.X))

	// The arrow registers on top of the instance row: resolving its cell must
	// hit the arrow, while the cell next to it hits the row.
	id, _, ok := reg.Resolve(r.X, r.Y)
	require.True(t, ok)
	assert.Equal(t, zones.TreeArrow("t-00"), id)
	id, _, ok = reg.Resolve(r.X+3, r.Y)
	require.True(t, ok)
	assert.Equal(t, zones.TreeInstance("t-00"), id)
}

// TestSidebarZonesClippedToRect: rows scrolled out of the window register no
// zones, and no registered zone extends past the pane rect (String()'s final
// clamp cuts partially fitting rows the same way).
func TestSidebarZonesClippedToRect(t *testing.T) {
	s := newTreeSidebar(t, 12)
	reg := zones.NewRegistry()
	s.SetZoneRegistry(reg)
	rect := layout.Rect{X: 0, Y: 0, W: 30, H: 12}
	s.SetRect(rect)
	s.SetSelectedInstance(0)

	reg.Reset()
	_ = s.String()

	registered := 0
	for _, id := range reg.IDs() {
		r, ok := reg.Find(id)
		require.True(t, ok)
		assert.LessOrEqual(t, r.Bottom(), rect.Bottom(), "zone %s must not extend past the pane", id)
		if title, isInst := zones.TreeInstanceTitle(id); isInst && title != "" {
			registered++
		}
	}
	assert.Less(t, registered, 12, "rows scrolled out of the window must not register zones")
	assert.Greater(t, registered, 0, "visible rows must register zones")
}

// TestSidebarClickPrimitives covers the click actions the mouse router calls:
// SelectTabRow retargets the active tab, ToggleInstanceTree drives the
// expansion policy, ClickHeader toggles the section.
func TestSidebarClickPrimitives(t *testing.T) {
	s := newTreeSidebar(t, 2)
	s.SetSize(40, 24)
	s.SetSelectedInstance(0)

	// Click a tab row: cursor lands on it, store's active tab follows.
	s.SelectTabRow("t-00", 1)
	sel := s.GetSelection()
	require.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, s.proj.ActiveTab())

	// Arrow on the selected instance: collapse, then expand again.
	s.ToggleInstanceTree("t-00")
	assert.Equal(t, 0, tabRowCount(s), "arrow click on the expanded selection collapses it")
	sel = s.GetSelection()
	assert.False(t, sel.IsTab, "the fold lands the cursor on the instance row")
	s.ToggleInstanceTree("t-00")
	assert.Equal(t, 2, tabRowCount(s), "second arrow click re-expands")

	// Arrow on a non-selected instance selects it (which auto-expands).
	s.ToggleInstanceTree("t-01")
	require.NotNil(t, s.GetSelectedInstance())
	assert.Equal(t, "t-01", s.GetSelectedInstance().Title)
	assert.Equal(t, 2, tabRowCount(s), "selecting via the arrow auto-expands the new selection")

	// Header click folds the whole section; a second click reopens it.
	s.ClickHeader()
	assert.Equal(t, 0, tabRowCount(s))
	assert.True(t, s.GetSelection().IsHeader)
	s.ClickHeader()
	assert.False(t, s.visibleItems[len(s.visibleItems)-1].IsHeader,
		"reopened section shows instance rows again")
}
