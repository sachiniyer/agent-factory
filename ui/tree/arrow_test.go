package tree

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

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

// TestArrowCellMatchesRenderedArrow pins ArrowCell — the sidebar's mouse hit
// target for the ▸/▾ glyph (#1024 R4) — against Render's actual output, so a
// prefix layout change moves the hit target with it or fails here.
func TestArrowCellMatchesRenderedArrow(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "arrow-pin", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)

	r := NewInstanceRenderer(&spin)
	r.SetWidth(30)
	r.SetIndexWidth(1)

	x, y, ok := ArrowCell(30)
	require.True(t, ok)

	for _, tc := range []struct {
		name     string
		expanded bool
		want     rune
	}{
		{"expanded", true, '▾'},
		{"collapsed", false, '▸'},
	} {
		out := r.Render(inst, 1, false, false, tc.expanded)
		lines := strings.Split(ansiEscape.ReplaceAllString(out, ""), "\n")
		require.Greater(t, len(lines), y)
		assert.Equal(t, tc.want, runeAtCell(lines[y], x),
			"%s: ArrowCell must point at the rendered arrow glyph", tc.name)
	}

	// Ultra-narrow widths drop the arrow from the prefix, and ArrowCell must
	// report that (the sidebar registers no arrow zone then).
	_, _, ok = ArrowCell(9)
	assert.False(t, ok, "no arrow cell at the narrow-width fallback")
}
