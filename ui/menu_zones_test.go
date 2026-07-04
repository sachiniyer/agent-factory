package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// cellSlice returns the terminal cells [from, to) of a plain line as a
// string. All menu hint glyphs are single-cell, so rune slicing suffices.
func cellSlice(line string, from, to int) string {
	runes := []rune(line)
	if from > len(runes) {
		return ""
	}
	if to > len(runes) {
		to = len(runes)
	}
	return string(runes[from:to])
}

// TestMenuHintZonesMatchRenderedHints (#1024 R4): every rendered hint
// registers a StatusHint zone, and the zone's cells hold exactly that hint's
// "key desc" text in the centered output — derived from the registry, so a
// change to the centering or separator math moves the zones with it or fails
// here.
func TestMenuHintZonesMatchRenderedHints(t *testing.T) {
	menu := NewMenu()
	reg := zones.NewRegistry()
	menu.SetZoneRegistry(reg)
	origin := layout.Point{X: 0, Y: 46}
	menu.SetOrigin(origin)
	menu.SetSize(120, 1)
	menu.SetState(StateEmpty)

	reg.Reset()
	out := xansi.Strip(menu.String())
	line := strings.Split(out, "\n")[0]

	for _, k := range defaultMenuOptions {
		binding := keys.GlobalKeyBindings[k]
		id := zones.StatusHint(binding.Keys()[0])
		r, ok := reg.Find(id)
		require.True(t, ok, "zone for hint %q; got %v", binding.Help().Key, reg.IDs())
		assert.Equal(t, origin.Y, r.Y, "hints render on the menu row")
		want := binding.Help().Key + " " + binding.Help().Desc
		assert.Equal(t, want, cellSlice(line, r.X-origin.X, r.X-origin.X+r.W),
			"the zone must cover exactly the rendered hint text")
	}
}

// TestMenuDroppedHintsRegisterNoZones: hints dropped by the narrow-width
// priority list must not leave clickable ghosts.
func TestMenuDroppedHintsRegisterNoZones(t *testing.T) {
	menu := NewMenu()
	reg := zones.NewRegistry()
	menu.SetZoneRegistry(reg)
	menu.SetOrigin(layout.Point{})
	menu.SetSize(30, 1) // too narrow for the full default row
	menu.SetState(StateEmpty)

	reg.Reset()
	_ = menu.String()

	// The drop order sheds "/" and "N" before help/quit; at 30 cols they are
	// gone, and their zones with them.
	_, hasSearch := reg.Find(zones.StatusHint("/"))
	assert.False(t, hasSearch, "dropped hints must not register zones")
	_, hasHelp := reg.Find(zones.StatusHint("?"))
	assert.True(t, hasHelp, "help is never dropped and keeps its zone")
	_, hasQuit := reg.Find(zones.StatusHint("q"))
	assert.True(t, hasQuit, "quit is never dropped and keeps its zone")
}

// TestMenuJumpTabHintHasNoZone: the "1-9" chip names nine keys, not one
// action, so it is deliberately not clickable.
func TestMenuJumpTabHintHasNoZone(t *testing.T) {
	menu := NewMenu()
	reg := zones.NewRegistry()
	menu.SetZoneRegistry(reg)
	menu.SetOrigin(layout.Point{})
	menu.SetSize(200, 1)
	menu.options = []keys.KeyName{keys.KeyJumpTab, keys.KeyHelp}
	menu.groups = []menuGroup{{start: 0, end: 2}}

	reg.Reset()
	_ = menu.String()
	for _, id := range reg.IDs() {
		key, ok := zones.StatusHintKey(id)
		require.True(t, ok)
		assert.NotEqual(t, "1", key, "the 1-9 jump chip must not register a zone")
	}
	_, hasHelp := reg.Find(zones.StatusHint("?"))
	assert.True(t, hasHelp)
}

// TestMenuInteractiveHintZone: while interactive the whole bar is the ctrl+]
// escape hatch, and its zone must carry the ctrl+] key so a click exits the
// mode through the normal key path.
func TestMenuInteractiveHintZone(t *testing.T) {
	menu := NewMenu()
	reg := zones.NewRegistry()
	menu.SetZoneRegistry(reg)
	menu.SetOrigin(layout.Point{Y: 30})
	menu.SetSize(100, 1)
	menu.SetInteractive(true)

	reg.Reset()
	_ = menu.String()
	r, ok := reg.Find(zones.StatusHint("ctrl+]"))
	require.True(t, ok, "the interactive bar's escape hatch must be clickable; got %v", reg.IDs())
	assert.Equal(t, 30, r.Y)
}
