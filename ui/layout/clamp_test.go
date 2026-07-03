package layout_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// requireExactSize asserts the ClampToRect contract: exactly h lines, each
// exactly w printable columns.
func requireExactSize(t *testing.T, s string, w, h int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	require.Len(t, lines, h)
	for i, line := range lines {
		require.Equal(t, w, lipgloss.Width(line), "line %d: %q", i, line)
	}
}

func TestClampToRectPadsUndersized(t *testing.T) {
	r := layout.Rect{W: 10, H: 4}

	out := layout.ClampToRect("hi", r)
	requireExactSize(t, out, 10, 4)
	lines := strings.Split(out, "\n")
	assert.Equal(t, "hi        ", lines[0])
	assert.Equal(t, strings.Repeat(" ", 10), lines[1], "missing lines pad as blanks")

	requireExactSize(t, layout.ClampToRect("", r), 10, 4)
}

func TestClampToRectTruncatesOversized(t *testing.T) {
	r := layout.Rect{W: 5, H: 2}

	out := layout.ClampToRect("abcdefghij\nklm\nnop\nqrs", r)
	requireExactSize(t, out, 5, 2)
	lines := strings.Split(out, "\n")
	assert.Equal(t, "abcde", lines[0], "over-wide line truncates to width")
	assert.Equal(t, "klm  ", lines[1], "over-tall input keeps the first H lines")
}

func TestClampToRectExactFitUnchanged(t *testing.T) {
	in := "abcde\nfghij"
	out := layout.ClampToRect(in, layout.Rect{W: 5, H: 2})
	assert.Equal(t, in, out, "exactly-sized content passes through untouched")
}

func TestClampToRectMultilineMixed(t *testing.T) {
	// Simultaneously over-wide, under-wide, and under-tall.
	out := layout.ClampToRect("way-too-long-line\nok", layout.Rect{W: 8, H: 3})
	requireExactSize(t, out, 8, 3)
	lines := strings.Split(out, "\n")
	assert.Equal(t, "way-too-", lines[0])
	assert.Equal(t, "ok      ", lines[1])
	assert.Equal(t, "        ", lines[2])
}

func TestClampToRectWideRunes(t *testing.T) {
	// Each CJK rune is 2 columns; "日本語のテスト" is 14 columns.
	out := layout.ClampToRect("日本語のテスト", layout.Rect{W: 5, H: 1})
	requireExactSize(t, out, 5, 1)
	assert.True(t, strings.HasPrefix(out, "日本"),
		"wide runes survive truncation intact: %q", out)
	assert.NotContains(t, out, "語",
		"the rune straddling the boundary is dropped, not split")

	// A wide rune straddling the edge leaves a 1-column gap that must be
	// padded to the exact width.
	requireExactSize(t, layout.ClampToRect("ああ", layout.Rect{W: 3, H: 1}), 3, 1)
}

func TestClampToRectANSIStyled(t *testing.T) {
	styled := "\x1b[31mred\x1b[0m plain \x1b[1;32mbold-green\x1b[0m"

	// Padding: ANSI is zero-width, styling preserved.
	out := layout.ClampToRect(styled, layout.Rect{W: 30, H: 2})
	requireExactSize(t, out, 30, 2)
	assert.Contains(t, out, "\x1b[31mred\x1b[0m", "styling survives padding")

	// Truncation mid-styled-run: width exact, style terminated so it cannot
	// bleed into padding or the neighboring region.
	out = layout.ClampToRect(styled, layout.Rect{W: 6, H: 1})
	requireExactSize(t, out, 6, 1)
	assert.Contains(t, out, "\x1b[31m", "styling survives truncation")
	assert.True(t, strings.HasSuffix(out, "\x1b[0m") || strings.HasSuffix(out, "m"),
		"truncated styled line is reset-terminated: %q", out)
	trailing := out[strings.LastIndex(out, "m")+1:]
	assert.Equal(t, strings.Repeat(" ", len(trailing)), trailing, "padding is unstyled")
}

func TestClampToRectEmptyRect(t *testing.T) {
	assert.Equal(t, "", layout.ClampToRect("content", layout.Rect{}))
	assert.Equal(t, "", layout.ClampToRect("content", layout.Rect{W: 5}))
	assert.Equal(t, "", layout.ClampToRect("content", layout.Rect{H: 5}))
	assert.Equal(t, "", layout.ClampToRect("content", layout.Rect{W: -2, H: 3}))
}

// TestClampToRectMatchesGridRegions ties the two halves of the §2.6
// contract together: content clamped to a solved region renders exactly
// that region's size.
func TestClampToRectMatchesGridRegions(t *testing.T) {
	l := layout.Grid{Panes: 2}.Solve(140, 40)
	require.False(t, l.Fallback)
	for id, r := range l.VisibleRegions() {
		out := layout.ClampToRect("some\ncontent\nfor "+id, r)
		requireExactSize(t, out, r.W, r.H)
	}
}
