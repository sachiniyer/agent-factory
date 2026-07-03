package layout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

func TestRectEmpty(t *testing.T) {
	assert.False(t, layout.Rect{X: 0, Y: 0, W: 1, H: 1}.Empty())
	assert.True(t, layout.Rect{}.Empty())
	assert.True(t, layout.Rect{W: 5}.Empty())
	assert.True(t, layout.Rect{H: 5}.Empty())
	assert.True(t, layout.Rect{W: -1, H: 5}.Empty())
	assert.True(t, layout.Rect{W: 5, H: -1}.Empty())
}

func TestRectRightBottom(t *testing.T) {
	r := layout.Rect{X: 3, Y: 4, W: 10, H: 20}
	assert.Equal(t, 13, r.Right())
	assert.Equal(t, 24, r.Bottom())
}

func TestRectContains(t *testing.T) {
	r := layout.Rect{X: 5, Y: 10, W: 4, H: 3} // cols 5..8, rows 10..12

	tests := []struct {
		name string
		p    layout.Point
		want bool
	}{
		{"top-left corner", layout.Point{X: 5, Y: 10}, true},
		{"interior", layout.Point{X: 7, Y: 11}, true},
		{"bottom-right inclusive cell", layout.Point{X: 8, Y: 12}, true},
		{"right edge exclusive", layout.Point{X: 9, Y: 10}, false},
		{"bottom edge exclusive", layout.Point{X: 5, Y: 13}, false},
		{"left of rect", layout.Point{X: 4, Y: 11}, false},
		{"above rect", layout.Point{X: 6, Y: 9}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.Contains(tt.p))
		})
	}

	assert.False(t, layout.Rect{X: 5, Y: 5}.Contains(layout.Point{X: 5, Y: 5}),
		"empty rect contains nothing")
}

func TestRectLocal(t *testing.T) {
	r := layout.Rect{X: 5, Y: 10, W: 4, H: 3}
	assert.Equal(t, layout.Point{X: 0, Y: 0}, r.Local(layout.Point{X: 5, Y: 10}))
	assert.Equal(t, layout.Point{X: 3, Y: 2}, r.Local(layout.Point{X: 8, Y: 12}))
}

func TestRectIntersects(t *testing.T) {
	base := layout.Rect{X: 0, Y: 0, W: 10, H: 10}

	tests := []struct {
		name  string
		other layout.Rect
		want  bool
	}{
		{"identical", base, true},
		{"contained", layout.Rect{X: 2, Y: 2, W: 3, H: 3}, true},
		{"corner overlap", layout.Rect{X: 9, Y: 9, W: 5, H: 5}, true},
		{"touching right edge", layout.Rect{X: 10, Y: 0, W: 5, H: 10}, false},
		{"touching bottom edge", layout.Rect{X: 0, Y: 10, W: 10, H: 5}, false},
		{"disjoint", layout.Rect{X: 20, Y: 20, W: 5, H: 5}, false},
		{"empty inside", layout.Rect{X: 5, Y: 5, W: 0, H: 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, base.Intersects(tt.other))
			assert.Equal(t, tt.want, tt.other.Intersects(base), "intersection is symmetric")
		})
	}
}

// requireTiles asserts that parts exactly tile whole: pairwise disjoint,
// each inside whole, and areas summing to whole's area.
func requireTiles(t *testing.T, whole layout.Rect, parts []layout.Rect) {
	t.Helper()
	area := 0
	for i, p := range parts {
		require.GreaterOrEqual(t, p.W, 0, "part %d has negative width", i)
		require.GreaterOrEqual(t, p.H, 0, "part %d has negative height", i)
		if p.Empty() {
			continue
		}
		require.GreaterOrEqual(t, p.X, whole.X, "part %d left of whole", i)
		require.GreaterOrEqual(t, p.Y, whole.Y, "part %d above whole", i)
		require.LessOrEqual(t, p.Right(), whole.Right(), "part %d overflows right", i)
		require.LessOrEqual(t, p.Bottom(), whole.Bottom(), "part %d overflows bottom", i)
		area += p.W * p.H
		for j := i + 1; j < len(parts); j++ {
			require.False(t, p.Intersects(parts[j]), "parts %d and %d overlap: %+v vs %+v", i, j, p, parts[j])
		}
	}
	require.Equal(t, whole.W*whole.H, area, "parts do not cover the whole rect")
}

func TestRectCutLeft(t *testing.T) {
	r := layout.Rect{X: 2, Y: 3, W: 10, H: 5}

	left, rem := r.CutLeft(4)
	assert.Equal(t, layout.Rect{X: 2, Y: 3, W: 4, H: 5}, left)
	assert.Equal(t, layout.Rect{X: 6, Y: 3, W: 6, H: 5}, rem)
	requireTiles(t, r, []layout.Rect{left, rem})

	left, rem = r.CutLeft(99)
	assert.Equal(t, r, left, "over-cut clamps to the whole rect")
	assert.True(t, rem.Empty())
	requireTiles(t, r, []layout.Rect{left, rem})

	left, rem = r.CutLeft(-3)
	assert.True(t, left.Empty(), "negative cut clamps to zero")
	assert.Equal(t, r, rem)
	requireTiles(t, r, []layout.Rect{left, rem})
}

func TestRectCutTop(t *testing.T) {
	r := layout.Rect{X: 2, Y: 3, W: 10, H: 5}

	top, rem := r.CutTop(2)
	assert.Equal(t, layout.Rect{X: 2, Y: 3, W: 10, H: 2}, top)
	assert.Equal(t, layout.Rect{X: 2, Y: 5, W: 10, H: 3}, rem)
	requireTiles(t, r, []layout.Rect{top, rem})

	top, rem = r.CutTop(99)
	assert.Equal(t, r, top)
	assert.True(t, rem.Empty())

	top, rem = r.CutTop(-1)
	assert.True(t, top.Empty())
	assert.Equal(t, r, rem)
}

func TestRectCutBottom(t *testing.T) {
	r := layout.Rect{X: 2, Y: 3, W: 10, H: 5}

	rem, bottom := r.CutBottom(2)
	assert.Equal(t, layout.Rect{X: 2, Y: 3, W: 10, H: 3}, rem)
	assert.Equal(t, layout.Rect{X: 2, Y: 6, W: 10, H: 2}, bottom)
	requireTiles(t, r, []layout.Rect{rem, bottom})

	rem, bottom = r.CutBottom(99)
	assert.True(t, rem.Empty())
	assert.Equal(t, r, bottom)

	rem, bottom = r.CutBottom(-1)
	assert.Equal(t, r, rem)
	assert.True(t, bottom.Empty())
}
