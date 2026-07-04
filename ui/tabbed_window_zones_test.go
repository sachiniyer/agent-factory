package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// TestTabbedWindowRegistersBodyAndHeaderZones (#1024 R4): the pane registers
// its whole rect as the body and the one-line header on top of it, with the
// header zone sitting on the row that actually renders the header text.
func TestTabbedWindowRegistersBodyAndHeaderZones(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	region := layout.PaneRegion(3)
	w.SetRegion(region)
	reg := zones.NewRegistry()
	w.SetZoneRegistry(reg)
	rect := layout.Rect{X: 30, Y: 2, W: 60, H: 20}
	w.SetRect(rect)

	reg.Reset()
	lines := plainLines(w.String())

	body, ok := reg.Find(zones.PaneBody(region))
	require.True(t, ok, "body zone; got %v", reg.IDs())
	assert.Equal(t, rect, body, "the body zone is the whole pane rect")

	header, ok := reg.Find(zones.PaneHeader(region))
	require.True(t, ok, "header zone")
	assert.Equal(t, layout.Rect{X: rect.X + 1, Y: rect.Y + 1, W: rect.W - 2, H: 1}, header,
		"the header sits inside the frame border")
	// The header zone's row renders the header text (nil binding here, so
	// the placeholder header).
	assert.Contains(t, lines[header.Y-rect.Y], "no session selected",
		"the header zone must sit on the rendered header line")

	// No live view bound: there must be no term zone to forward into.
	_, hasTerm := reg.Find(zones.PaneTerm(region))
	assert.False(t, hasTerm, "capture panes must not advertise a terminal grid")

	// Precedence: a point on the header row resolves to the header, a point
	// below it to the body.
	id, _, ok := reg.Resolve(header.X+2, header.Y)
	require.True(t, ok)
	assert.Equal(t, zones.PaneHeader(region), id)
	id, _, ok = reg.Resolve(header.X+2, header.Y+3)
	require.True(t, ok)
	assert.Equal(t, zones.PaneBody(region), id)
}

// TestTabbedWindowRegistersTermZoneWhileLive: exactly while the live embedded
// terminal is what renders, the content grid registers as the term zone —
// whose zone-local coordinates are emulator grid cells (the interactive
// forwarding target). Scroll mode swaps back to the capture viewport and the
// term zone must vanish with it.
func TestTabbedWindowRegistersTermZoneWhileLive(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	region := layout.PaneRegion(0)
	w.SetRegion(region)
	reg := zones.NewRegistry()
	w.SetZoneRegistry(reg)
	rect := layout.Rect{X: 25, Y: 0, W: 50, H: 16}
	w.SetRect(rect)
	w.SetLive(&fakeLiveView{content: "LIVE"})

	reg.Reset()
	_ = w.String()

	term, ok := reg.Find(zones.PaneTerm(region))
	require.True(t, ok, "live pane must register its terminal grid; got %v", reg.IDs())
	iw, ih := w.innerSize()
	assert.Equal(t, layout.Rect{X: rect.X + 1, Y: rect.Y + 2, W: iw, H: ih}, term,
		"the term zone is the content area inside frame + header")

	// The grid's local coordinates are emulator cells: its top-left resolves
	// to (0, 0).
	id, local, ok := reg.Resolve(term.X, term.Y)
	require.True(t, ok)
	assert.Equal(t, zones.PaneTerm(region), id)
	assert.Equal(t, layout.Point{X: 0, Y: 0}, local)

	// Unbind: the term zone must not survive the live view.
	w.SetLive(nil)
	reg.Reset()
	_ = w.String()
	_, hasTerm := reg.Find(zones.PaneTerm(region))
	assert.False(t, hasTerm, "no term zone once the live view is unbound")
}

// TestTabbedWindowNoZonesWhenHiddenOrUnwired: a zero rect (auto-hidden pane)
// registers nothing, and a window never given a region (defensive) skips
// registration rather than emitting unparseable ids.
func TestTabbedWindowNoZonesWhenHiddenOrUnwired(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	w.SetRegion(layout.PaneRegion(1))
	reg := zones.NewRegistry()
	w.SetZoneRegistry(reg)
	w.SetRect(layout.Rect{})
	reg.Reset()
	_ = w.String()
	assert.Empty(t, reg.IDs(), "a hidden pane must not register hit zones")

	w2 := NewTabbedWindow(NewTabPane(), nil)
	w2.SetZoneRegistry(reg)
	w2.SetRect(layout.Rect{W: 40, H: 12})
	reg.Reset()
	_ = w2.String()
	assert.Empty(t, reg.IDs(), "a window with no region must not register zones")
}
