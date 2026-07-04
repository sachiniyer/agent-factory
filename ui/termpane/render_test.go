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

	lines := gridLines(t, renderGrid(emu, 10, 3, cursorNone), 10, 3)
	assert.Equal(t, "hi        ", ansi.Strip(lines[0]), "content padded to full width")
	assert.Equal(t, strings.Repeat(" ", 10), ansi.Strip(lines[1]), "empty rows render as blanks")
}

func TestRenderGridCarriesStylesAndResets(t *testing.T) {
	emu := vt.NewEmulator(12, 2)
	_, err := emu.Write([]byte("\x1b[31mred\x1b[m plain"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 12, 2, cursorNone), 12, 2)
	assert.Contains(t, lines[0], "31m", "SGR color must survive the grid round-trip")
	assert.Equal(t, "red plain   ", ansi.Strip(lines[0]))
}

func TestRenderGridResetsStyleActiveAtEndOfRow(t *testing.T) {
	// A row styled through its last cell must end with a reset so the style
	// can never bleed into the host TUI's chrome to the right of the pane.
	emu := vt.NewEmulator(5, 1)
	_, err := emu.Write([]byte("\x1b[31mABCDE"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 5, 1, cursorNone), 5, 1)
	assert.True(t, strings.HasSuffix(lines[0], "\x1b[m"), "row must end with a reset, got %q", lines[0])
}

func TestRenderGridWideCharacters(t *testing.T) {
	emu := vt.NewEmulator(10, 2)
	_, err := emu.Write([]byte("日本語"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 10, 2, cursorNone), 10, 2)
	assert.Equal(t, "日本語    ", ansi.Strip(lines[0]), "3 wide glyphs occupy 6 cells + 4 blanks")
}

func TestRenderGridClipsAndPadsAroundResize(t *testing.T) {
	// A grid larger than the requested rect (the transient state right
	// before Resize catches up) clips; a smaller one pads. Both must hold
	// the exact width x height contract.
	emu := vt.NewEmulator(12, 4)
	_, err := emu.Write([]byte("abcdefghijkl\r\nsecond"))
	require.NoError(t, err)

	lines := gridLines(t, renderGrid(emu, 6, 2, cursorNone), 6, 2)
	assert.Equal(t, "abcdef", ansi.Strip(lines[0]))
	assert.Equal(t, "second", ansi.Strip(lines[1]))

	gridLines(t, renderGrid(emu, 20, 6, cursorNone), 20, 6)
}

func TestRenderGridBlanksWideGlyphStraddlingClipBoundary(t *testing.T) {
	emu := vt.NewEmulator(10, 1)
	_, err := emu.Write([]byte("日本語")) // cells 0-5, glyph starts at 4
	require.NoError(t, err)

	// Clipping at width 5 lands mid-glyph: the straddling glyph must blank,
	// never overflow the row.
	lines := gridLines(t, renderGrid(emu, 5, 1, cursorNone), 5, 1)
	assert.Equal(t, "日本 ", ansi.Strip(lines[0]))
}

func TestRenderGridDegenerateSizes(t *testing.T) {
	emu := vt.NewEmulator(10, 2)
	assert.Empty(t, renderGrid(emu, 0, 2, cursorNone))
	assert.Empty(t, renderGrid(emu, 10, 0, cursorNone))
	assert.Empty(t, renderGrid(emu, -1, -1, cursorNone))
}

func TestRenderGridCursorOverlay(t *testing.T) {
	emu := vt.NewEmulator(10, 2)
	_, err := emu.Write([]byte("ab"))
	require.NoError(t, err)

	// Cursor sits at (2,0) after "ab". The overlay flips reverse video on
	// exactly that cell; without it (nav mode / PR 1 behavior) no reverse
	// attribute appears anywhere.
	pos := emu.CursorPosition()
	withCursor := renderGrid(emu, 10, 2, cursorAt{x: pos.X, y: pos.Y, show: true})
	assert.Contains(t, withCursor, "\x1b[7m", "cursor cell must render reverse-video")
	assert.NotContains(t, renderGrid(emu, 10, 2, cursorNone), "\x1b[7m")

	// A cell already reverse-video under the cursor flips BACK, so the
	// cursor stays visible on reverse-styled content (e.g. a selection bar).
	emu2 := vt.NewEmulator(10, 1)
	_, err = emu2.Write([]byte("\x1b[7mXY"))
	require.NoError(t, err)
	lines := gridLines(t, renderGrid(emu2, 10, 1, cursorAt{x: 0, y: 0, show: true}), 10, 1)
	assert.NotContains(t, strings.Split(lines[0], "X")[0], "\x1b[7m",
		"reverse content under the cursor must flip back to normal")
}

func TestTermPaneCursorVisibilityTracksDECTCM(t *testing.T) {
	// The inner application hides the cursor (\x1b[?25l — htop, spinners):
	// the interactive overlay must follow, or a phantom block cursor floats
	// over the pane.
	tp := startScript(t, "printf 'hide\\033[?25l'; sleep 30", 20, 4)
	waitForRender(t, tp, 20, 4, "hide")
	assert.NotContains(t, tp.Render(20, 4, true), "\x1b[7m",
		"hidden cursor must not render even when the pane asks for it")

	tp2 := startScript(t, "printf shown; sleep 30", 20, 4)
	waitForRender(t, tp2, 20, 4, "shown")
	assert.Contains(t, tp2.Render(20, 4, true), "\x1b[7m",
		"visible cursor must overlay while interactive")
	assert.NotContains(t, tp2.Render(20, 4, false), "\x1b[7m",
		"nav-mode render stays cursorless")
}
