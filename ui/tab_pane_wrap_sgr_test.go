package ui

import (
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// wrapSGRCapture is what tmux 3.4's rows capture (`capture-pane -p -e`, no -J)
// returns at 80 columns for:
//
//	printf '\e[31m%s\e[0m\n' <170 x>       a red line that soft-wraps twice
//	printf '\e[1;33m%s-tail\e[0m\n' <90 y>  a bold yellow line that wraps once
//	printf '\e[34mblue\e[0m\nplain\n'      hard newline after tmux's own close
//	printf '\e[32mgreen\e[0m\nreset\n'     hard newline after an explicit reset
//
// Measured on an isolated tmux 3.4 server: tmux opens a soft-wrapped line's SGR
// once, on its first row, and closes it only at the line's end, while a hard
// newline row is closed at its own end (#7419).
var wrapSGRCapture = strings.Join([]string{
	"\x1b[31m" + strings.Repeat("x", 80),
	strings.Repeat("x", 80),
	strings.Repeat("x", 10) + "\x1b[39m",
	"\x1b[1m\x1b[33m" + strings.Repeat("y", 80),
	strings.Repeat("y", 10) + "-tail\x1b[0m",
	"\x1b[34mblue\x1b[39m",
	"plain",
	"\x1b[32mgreen\x1b[0m",
	"reset",
}, "\n") + "\n"

// paintPane draws a TabPane frame into a terminal emulator, so the assertions
// read the colour a user sees in each cell instead of re-parsing the escapes.
func paintPane(t *testing.T, frame string, w, h int) *vt.Emulator {
	t.Helper()
	emu := vt.NewEmulator(w, h)
	_, err := emu.WriteString(strings.ReplaceAll(frame, "\n", "\r\n"))
	require.NoError(t, err)
	return emu
}

// cellsOf returns the style of every cell holding glyph, row by row (rows with
// none are skipped), reading only the glyph's own run so "-tail" is excluded.
func cellsOf(emu *vt.Emulator, w, h int, glyph string) [][]uv.Style {
	var rows [][]uv.Style
	for y := 0; y < h; y++ {
		var row []uv.Style
		for x := 0; x < w; x++ {
			if c := emu.CellAt(x, y); c != nil && c.Content == glyph {
				row = append(row, c.Style)
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

func sameColour(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// rowStartingWith returns the styles of the cells on the first row whose text
// starts with prefix.
func rowStartingWith(t *testing.T, emu *vt.Emulator, w, h int, prefix string) []uv.Style {
	t.Helper()
	for y := 0; y < h; y++ {
		var text strings.Builder
		var styles []uv.Style
		for x := 0; x < w; x++ {
			if c := emu.CellAt(x, y); c != nil && c.Content != "" {
				text.WriteString(c.Content)
				styles = append(styles, c.Style)
			}
		}
		if strings.HasPrefix(text.String(), prefix) {
			return styles[:len(prefix)]
		}
	}
	t.Fatalf("no row starts with %q", prefix)
	return nil
}

func requireWrapColours(t *testing.T, emu *vt.Emulator, w, h int, wantRedRows int) {
	t.Helper()
	red := cellsOf(emu, w, h, "x")
	require.Len(t, red, wantRedRows, "precondition: the red line's rows are on screen")
	for i, row := range red {
		for x, style := range row {
			require.True(t, sameColour(style.Fg, xansi.Red),
				"red line, drawn row %d, cell %d: a soft-wrapped continuation keeps the colour its first row opened, got fg %v", i+1, x+1, style.Fg)
		}
	}
	for i, row := range cellsOf(emu, w, h, "y") {
		for x, style := range row {
			require.True(t, sameColour(style.Fg, xansi.Yellow) && style.Attrs&uv.AttrBold != 0,
				"bold yellow line, drawn row %d, cell %d: got fg %v attrs %b", i+1, x+1, style.Fg, style.Attrs)
		}
	}
}

func requireHardNewlinesDoNotInherit(t *testing.T, emu *vt.Emulator, w, h int) {
	t.Helper()
	for prefix, previous := range map[string]color.Color{"plain": xansi.Blue, "reset": xansi.Green} {
		for x, style := range rowStartingWith(t, emu, w, h, prefix) {
			require.False(t, sameColour(style.Fg, previous),
				"%q, cell %d: a row after a hard newline must not inherit the previous row's closed colour", prefix, x+1)
			require.Zero(t, style.Attrs&uv.AttrBold, "%q, cell %d: no attribute leaks either", prefix, x+1)
		}
	}
}

// TestCapturedPaneKeepsColourAcrossASoftWrap pins #7419. Since #7417 the pane
// draws the session's rows rather than lines joined across a soft wrap. tmux
// opens a wrapped line's colour once, on its first row, and the pane rendered
// each row on its own, so the continuation rows fell back to the default ink.
// Every drawn row must start in the SGR state in effect at its first cell, and
// a row after a hard newline must not inherit a colour the row before it closed.
func TestCapturedPaneKeepsColourAcrossASoftWrap(t *testing.T) {
	// The test binary has no TTY, so without a colour profile lipgloss styles
	// nothing and adds none of the per-row resets production draws with.
	forceProfile(t, termenv.TrueColor)
	const w, h = 80, 12
	inst := makeShellInstance(t, "wrapsgr", "ignored")
	defer func() { _ = inst.Kill() }()
	src := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		return hostPreview(wrapSGRCapture), nil
	}

	for _, tab := range []int{0, 1} {
		p := NewTabPane(src)
		p.SetSize(w, h)
		require.NoError(t, p.UpdateContent(inst, tab))
		emu := paintPane(t, p.String(), w, h)
		requireWrapColours(t, emu, w, h, 3)
		requireHardNewlinesDoNotInherit(t, emu, w, h)
	}
}

// TestScrolledPaneKeepsColourAcrossASoftWrap is the scroll viewport half of
// #7419. The viewport shows a window of the history, and a window that starts
// on a continuation row holds no SGR from the row that opened the colour.
func TestScrolledPaneKeepsColourAcrossASoftWrap(t *testing.T) {
	// The test binary has no TTY, so without a colour profile lipgloss styles
	// nothing and adds none of the per-row resets production draws with.
	forceProfile(t, termenv.TrueColor)
	const w, h = 80, 12
	p, inst := enterScrollOver(t, "wrapsgrscroll", wrapSGRCapture, w, h)
	defer func() { _ = inst.Kill() }()
	emu := paintPane(t, p.String(), w, h)
	requireWrapColours(t, emu, w, h, 3)
	requireHardNewlinesDoNotInherit(t, emu, w, h)

	// A two-row window over the red line's continuation rows only.
	const tall = 2
	window := strings.Join([]string{
		"\x1b[31m" + strings.Repeat("x", 80),
		strings.Repeat("x", 80),
		strings.Repeat("x", 10) + "\x1b[39m",
		"end",
	}, "\n") + "\n"
	p, inst = enterScrollOver(t, "wrapsgrwindow", window, w, tall)
	defer func() { _ = inst.Kill() }()
	emu = paintPane(t, p.String(), w, tall)
	var widths []int
	for _, row := range cellsOf(emu, w, tall, "x") {
		widths = append(widths, len(row))
	}
	require.Equal(t, []int{80, 10}, widths,
		"precondition: the window holds the red line's last two rows, not the row that opened the colour")
	requireWrapColours(t, emu, w, tall, 2)
}
