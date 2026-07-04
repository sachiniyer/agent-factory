package termpane

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gridLines splits a rendered grid and asserts the exact width x height
// contract every renderGrid caller (the pane clamp discipline) relies on.
func gridLines(t *testing.T, grid string, width, height int) []string {
	t.Helper()
	lines := strings.Split(grid, "\n")
	require.Len(t, lines, height, "grid must render exactly %d lines", height)
	for i, line := range lines {
		require.Equalf(t, width, ansi.StringWidth(line), "line %d must be exactly %d cells", i, width)
	}
	return lines
}

func TestRenderGridPlainTextAndPadding(t *testing.T) {
	emu := vt.NewEmulator(10, 3)
	_, err := emu.Write([]byte("hi"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 10, 3), 10, 3)
	assert.Equal(t, "hi        ", ansi.Strip(lines[0]), "content padded to full width")
	assert.Equal(t, strings.Repeat(" ", 10), ansi.Strip(lines[1]), "empty rows render as blanks")
}

func TestRenderGridCarriesStylesAndResets(t *testing.T) {
	emu := vt.NewEmulator(12, 2)
	_, err := emu.Write([]byte("\x1b[31mred\x1b[m plain"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 12, 2), 12, 2)
	assert.Contains(t, lines[0], "31m", "SGR color must survive the grid round-trip")
	assert.Equal(t, "red plain   ", ansi.Strip(lines[0]))
}

func TestRenderGridResetsStyleActiveAtEndOfRow(t *testing.T) {
	// A row styled through its last cell must end with a reset so the style
	// can never bleed into the host TUI's chrome to the right of the pane.
	emu := vt.NewEmulator(5, 1)
	_, err := emu.Write([]byte("\x1b[31mABCDE"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 5, 1), 5, 1)
	assert.True(t, strings.HasSuffix(lines[0], "\x1b[m"), "row must end with a reset, got %q", lines[0])
}

func TestRenderGridWideCharacters(t *testing.T) {
	emu := vt.NewEmulator(10, 2)
	_, err := emu.Write([]byte("日本語"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 10, 2), 10, 2)
	assert.Equal(t, "日本語    ", ansi.Strip(lines[0]), "3 wide glyphs occupy 6 cells + 4 blanks")
}

func TestRenderGridClipsAndPadsAroundResize(t *testing.T) {
	// A grid larger than the requested rect (the transient state right
	// before Resize catches up) clips; a smaller one pads. Both must hold
	// the exact width x height contract.
	emu := vt.NewEmulator(12, 4)
	_, err := emu.Write([]byte("abcdefghijkl\r\nsecond"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 6, 2), 6, 2)
	assert.Equal(t, "abcdef", ansi.Strip(lines[0]))
	assert.Equal(t, "second", ansi.Strip(lines[1]))

	gridLines(t, renderGrid(emu, 20, 6), 20, 6)
}

func TestRenderGridBlanksWideGlyphStraddlingClipBoundary(t *testing.T) {
	emu := vt.NewEmulator(10, 1)
	_, err := emu.Write([]byte("日本語")) // cells 0-5, glyph starts at 4
	require.NoError(t, err)

	// Clipping at width 5 lands mid-glyph: the straddling glyph must blank,
	// never overflow the row.
	lines := gridLines(t, renderGrid(emu, 5, 1), 5, 1)
	assert.Equal(t, "日本 ", ansi.Strip(lines[0]))
}

func TestRenderGridDegenerateSizes(t *testing.T) {
	emu := vt.NewEmulator(10, 2)
	assert.Empty(t, renderGrid(emu, 0, 2))
	assert.Empty(t, renderGrid(emu, 10, 0))
	assert.Empty(t, renderGrid(emu, -1, -1))
}
