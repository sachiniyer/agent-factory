package overlay

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

func keyEnter() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

// ----------------------------------------------------------------------------
// Overlay button zones (#1024 R4): clicking the confirmation's y/n words or a
// selection/search row is equivalent to the key. The zones are derived by
// scanning the rendered output, and these tests verify each zone lands on the
// rendered text it stands for.
// ----------------------------------------------------------------------------

// cellSliceAt returns the w terminal cells starting at absolute column x of a
// rendered line, given the overlay was registered at origin.
func cellSliceAt(line string, x, w int) string {
	plain := xansi.Strip(line)
	// All confirm/selection glyphs are single-cell, so walk runes by width.
	col, start := 0, -1
	var out []rune
	for i, r := range []rune(plain) {
		if col == x && start < 0 {
			start = i
		}
		if start >= 0 && col < x+w {
			out = append(out, r)
		}
		col += runewidth.RuneWidth(r)
	}
	return string(out)
}

func TestConfirmationOverlayRegistersYesNoZones(t *testing.T) {
	c := NewConfirmationOverlay("[!] Kill session 'alpha'?")
	c.SetWidth(50)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 12, Y: 5}
	c.RegisterZones(reg, origin)

	lines := strings.Split(c.Render(), "\n")

	yes, ok := reg.Find(zones.OverlayConfirmYes)
	require.True(t, ok, "yes zone; got %v", reg.IDs())
	no, ok := reg.Find(zones.OverlayConfirmNo)
	require.True(t, ok, "no zone")

	assert.Equal(t, yes.Y, no.Y, "both buttons sit on the instruction line")
	line := lines[yes.Y-origin.Y]
	assert.Equal(t, "y to confirm", cellSliceAt(line, yes.X-origin.X, yes.W),
		"the yes zone covers exactly its rendered words")
	assert.Equal(t, "n or esc to cancel", cellSliceAt(line, no.X-origin.X, no.W),
		"the no zone covers exactly its rendered words")

	// Resolve precedence sanity: a click on each zone resolves to it.
	id, _, ok := reg.Resolve(yes.X, yes.Y)
	require.True(t, ok)
	assert.Equal(t, zones.OverlayConfirmYes, id)
	id, _, ok = reg.Resolve(no.X+2, no.Y)
	require.True(t, ok)
	assert.Equal(t, zones.OverlayConfirmNo, id)
}

// TestConfirmationOverlayZonesFollowCustomKeys: overlays with a custom
// confirm key register the zone over the custom instruction text.
func TestConfirmationOverlayZonesFollowCustomKeys(t *testing.T) {
	c := NewConfirmationOverlay("proceed?")
	c.SetWidth(50)
	c.SetConfirmKey("d")
	reg := zones.NewRegistry()
	c.RegisterZones(reg, layout.Point{})

	yes, ok := reg.Find(zones.OverlayConfirmYes)
	require.True(t, ok)
	line := strings.Split(c.Render(), "\n")[yes.Y]
	assert.Equal(t, "d to confirm", cellSliceAt(line, yes.X, yes.W))
}

func TestSelectionOverlayRegistersRowZones(t *testing.T) {
	items := []string{"claude", "aider", "codex"}
	s := NewSelectionOverlay("Select Program", items)
	s.SetWidth(50)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 8, Y: 4}
	s.RegisterZones(reg, origin)

	lines := strings.Split(s.Render(), "\n")
	for i, item := range items {
		r, ok := reg.Find(zones.OverlaySelectRow(i))
		require.True(t, ok, "row zone for %q; got %v", item, reg.IDs())
		assert.Equal(t, origin.X, r.X, "row zones span the full overlay width")
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), item,
			"row %d's zone must sit on the line rendering %q", i, item)
	}
}

func TestSearchOverlayRegistersRowZonesAndSetSelectedIndex(t *testing.T) {
	instances := []*session.Instance{
		{Title: "alpha"},
		{Title: "alpha-2"},
		{Title: "beta"},
	}
	s := NewSearchOverlay(instances)
	reg := zones.NewRegistry()
	origin := layout.Point{X: 10, Y: 3}
	s.RegisterZones(reg, origin)

	lines := strings.Split(s.Render(), "\n")
	for i, inst := range instances {
		r, ok := reg.Find(zones.OverlaySearchRow(i))
		require.True(t, ok, "row zone for %q; got %v", inst.Title, reg.IDs())
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), inst.Title,
			"result %d's zone must sit on the line rendering %q", i, inst.Title)
	}
	// "alpha" is a prefix of "alpha-2": the ordered scan must not have bound
	// both zones to the same line.
	r0, _ := reg.Find(zones.OverlaySearchRow(0))
	r1, _ := reg.Find(zones.OverlaySearchRow(1))
	assert.NotEqual(t, r0.Y, r1.Y, "prefix-colliding titles must map to distinct rows")

	// SetSelectedIndex is the click primitive: it moves the selection the
	// subsequent enter submits.
	s.SetSelectedIndex(2)
	require.True(t, s.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[2], s.GetSelectedInstance())

	// Out-of-range clicks are refused.
	s2 := NewSearchOverlay(instances)
	s2.SetSelectedIndex(99)
	require.True(t, s2.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[0], s2.GetSelectedInstance())
}

// TestSearchOverlayScrolledWindowRegistersVisibleRows pins the Greptile P1 on
// the original mouse PR (#1086): once the selection scrolls past the first
// page, Render windows the results — and the registered zones must be the
// rows actually on screen, keyed by their FULL-LIST indices, instead of
// matching the visible rows against results[0] and registering nothing.
func TestSearchOverlayScrolledWindowRegistersVisibleRows(t *testing.T) {
	var instances []*session.Instance
	for i := 1; i <= 12; i++ {
		instances = append(instances, &session.Instance{Title: sessionTitle(i)})
	}
	s := NewSearchOverlay(instances)
	// Walk the selection to the last result: the visible window is now
	// results[2..12).
	for range instances {
		_ = s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	reg := zones.NewRegistry()
	origin := layout.Point{X: 5, Y: 2}
	s.RegisterZones(reg, origin)

	_, hasFirst := reg.Find(zones.OverlaySearchRow(0))
	assert.False(t, hasFirst, "rows scrolled above the window must not register")

	lines := strings.Split(s.Render(), "\n")
	for i := 2; i < 12; i++ {
		r, ok := reg.Find(zones.OverlaySearchRow(i))
		require.True(t, ok, "visible row %d must register; got %v", i, reg.IDs())
		assert.Contains(t, xansi.Strip(lines[r.Y-origin.Y]), sessionTitle(i+1),
			"row %d's zone must sit on the line rendering it", i)
	}

	// The click primitive round-trips: clicking the row registered for index
	// 5 selects session-06.
	s.SetSelectedIndex(5)
	require.True(t, s.HandleKeyPress(keyEnter()))
	assert.Same(t, instances[5], s.GetSelectedInstance())
}

func sessionTitle(i int) string {
	return "session-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}
