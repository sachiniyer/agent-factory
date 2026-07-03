package zones_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

func TestRegistryResolveBasic(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Register("tree", layout.Rect{X: 0, Y: 0, W: 30, H: 40})
	reg.Register("paneA", layout.Rect{X: 30, Y: 0, W: 70, H: 40})

	id, local, ok := reg.Resolve(5, 7)
	require.True(t, ok)
	assert.Equal(t, "tree", id)
	assert.Equal(t, layout.Point{X: 5, Y: 7}, local)

	id, local, ok = reg.Resolve(35, 10)
	require.True(t, ok)
	assert.Equal(t, "paneA", id)
	assert.Equal(t, layout.Point{X: 5, Y: 10}, local, "local coords are zone-relative")
}

func TestRegistryResolveBoundaries(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Register("z", layout.Rect{X: 10, Y: 5, W: 20, H: 10})

	// Half-open edges: last inside cell hits, first outside cell misses.
	_, local, ok := reg.Resolve(10, 5)
	require.True(t, ok, "top-left corner is inside")
	assert.Equal(t, layout.Point{X: 0, Y: 0}, local)

	_, local, ok = reg.Resolve(29, 14)
	require.True(t, ok, "bottom-right cell is inside")
	assert.Equal(t, layout.Point{X: 19, Y: 9}, local)

	_, _, ok = reg.Resolve(30, 5)
	assert.False(t, ok, "right edge is exclusive")
	_, _, ok = reg.Resolve(10, 15)
	assert.False(t, ok, "bottom edge is exclusive")
	_, _, ok = reg.Resolve(9, 5)
	assert.False(t, ok)
}

func TestRegistryResolveMiss(t *testing.T) {
	reg := zones.NewRegistry()

	id, local, ok := reg.Resolve(5, 5)
	assert.False(t, ok, "empty registry misses everywhere")
	assert.Equal(t, "", id)
	assert.Equal(t, layout.Point{}, local)

	reg.Register("z", layout.Rect{X: 0, Y: 0, W: 10, H: 10})
	id, _, ok = reg.Resolve(50, 50)
	assert.False(t, ok)
	assert.Equal(t, "", id)
}

func TestRegistryOverlapLastRegisteredWins(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Register("pane", layout.Rect{X: 0, Y: 0, W: 50, H: 20})
	reg.Register("pane:header", layout.Rect{X: 0, Y: 0, W: 50, H: 1})
	reg.Register("pane:header:close", layout.Rect{X: 48, Y: 0, W: 2, H: 1})

	// Registration order is paint order: the innermost, last-registered
	// target is on top.
	id, local, ok := reg.Resolve(48, 0)
	require.True(t, ok)
	assert.Equal(t, "pane:header:close", id)
	assert.Equal(t, layout.Point{X: 0, Y: 0}, local, "local coords are relative to the winning zone")

	id, _, ok = reg.Resolve(10, 0)
	require.True(t, ok)
	assert.Equal(t, "pane:header", id, "header beats the pane beneath it")

	id, _, ok = reg.Resolve(10, 5)
	require.True(t, ok)
	assert.Equal(t, "pane", id, "uncovered pane body still hits the pane")
}

func TestRegistryEmptyRectNeverHit(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Register("ghost", layout.Rect{X: 5, Y: 5, W: 0, H: 10})
	_, _, ok := reg.Resolve(5, 5)
	assert.False(t, ok)
}

func TestRegistryReset(t *testing.T) {
	reg := zones.NewRegistry()
	reg.Register("a", layout.Rect{X: 0, Y: 0, W: 10, H: 10})
	_, _, ok := reg.Resolve(5, 5)
	require.True(t, ok)

	// Per-frame rebuild: Reset clears everything…
	reg.Reset()
	_, _, ok = reg.Resolve(5, 5)
	assert.False(t, ok, "Reset clears all registrations")

	// …and the next frame's registrations take effect cleanly.
	reg.Register("b", layout.Rect{X: 0, Y: 0, W: 10, H: 10})
	id, _, ok := reg.Resolve(5, 5)
	require.True(t, ok)
	assert.Equal(t, "b", id)
}

// TestRegistryFromGridRegions exercises the intended per-frame usage:
// register every visible region of a solved layout and verify any screen
// cell resolves to exactly the region that contains it.
func TestRegistryFromGridRegions(t *testing.T) {
	l := layout.Grid{Panes: 2}.Solve(140, 40)
	require.False(t, l.Fallback)

	reg := zones.NewRegistry()
	for id, r := range l.VisibleRegions() {
		reg.Register(id, r)
	}

	for y := 0; y < l.Height; y++ {
		for x := 0; x < l.Width; x++ {
			id, local, ok := reg.Resolve(x, y)
			require.True(t, ok, "cell (%d,%d) resolves — regions tile the screen", x, y)
			r := l.VisibleRegions()[id]
			require.True(t, r.Contains(layout.Point{X: x, Y: y}))
			require.Equal(t, layout.Point{X: x - r.X, Y: y - r.Y}, local)
		}
	}
}
