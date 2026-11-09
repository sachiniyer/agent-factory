package termpane

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/terminal"
)

// A viewer pane keeps the pane's REAL size and crops it into a shorter box
// (#4480). On the alternate screen the cursor is not a working edge: a
// full-screen program parks it wherever it likes. `watch` leaves it in the
// bottom-right corner of a screen whose content is three rows at the top
// (measured on tmux: cursor_y=23 cursor_x=79 on an 80x24 pane). A crop that
// anchors on that cursor shows only blank rows, so the committed pane renders
// empty until something resizes the pane (#7037).

// watchScreen is the visible grid `watch -n1 uptime` draws on an 80x24 pane, as
// tmux's capture-pane reports it: the header row, a blank row, the command's
// output, and nothing below.
var watchScreen = []string{
	"Every 1.0s: uptime                   devbox: Mon Nov  9 05:08:57 2026",
	"",
	" 05:08:57 up 195 days,  8:16,  0 user,  load average: 5.20, 6.09, 5.94",
}

// repaintBytes builds a fresh-subscriber repaint the way the daemon does for a
// grid capture (session/ptybroker_repaint.go buildRepaint): restore the
// ownership modes, clear, pin each captured row at its own absolute line, then
// put the cursor where the pane's program left it (0-based row/col).
func repaintBytes(modes terminal.Modes, rows []string, cursorRow, cursorCol int) string {
	var b strings.Builder
	b.Write(modes.RestoreSequence())
	b.WriteString("\x1b[2J")
	for i, row := range rows {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K%s", i+1, row)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", cursorRow+1, cursorCol+1)
	return b.String()
}

// altScreenDraw is the same screen arriving as live output: a program entering
// the alternate screen, drawing its rows, and parking the cursor.
func altScreenDraw(rows []string, cursorRow, cursorCol int) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	for i, row := range rows {
		fmt.Fprintf(&b, "\x1b[%d;1H%s", i+1, row)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", cursorRow+1, cursorCol+1)
	return b.String()
}

// newTallViewer starts a viewer whose box is viewW x viewH and waits for the
// broker's echo to size its emulator to the pane's real paneW x paneH.
func newTallViewer(t *testing.T, viewW, viewH int, paneRows, paneCols uint16) (*TermPane, *fakeStream) {
	t.Helper()
	tp, s := newSingleStreamPane(t, viewW, viewH)
	s.feedResize(paneRows, paneCols)
	require.Eventually(t, func() bool {
		tp.gridMu.RLock()
		defer tp.gridMu.RUnlock()
		return tp.emu.Height() == int(paneRows) && tp.emu.Width() == int(paneCols)
	}, 2*time.Second, 5*time.Millisecond, "the echo must size the viewer's emulator to the pane")
	return tp, s
}

// TestViewerShowsAltScreenContentAboveAParkedCursor is the #7037 repro. The
// committed pane is a viewer of an 80x24 pane through a 10-row box, and the
// program on the alternate screen has parked its cursor on the bottom row,
// below every row it drew. The pane must show what the program drew, as soon as
// the screen arrives — whether that is the subscribe-time repaint of an
// already-running program that will never draw again (an idle `less`, a
// `watch` between ticks), or the program's own live output.
func TestViewerShowsAltScreenContentAboveAParkedCursor(t *testing.T) {
	cases := []struct {
		name string
		send func(s *fakeStream)
	}{
		{
			name: "fresh-subscribe repaint of an idle program",
			send: func(s *fakeStream) {
				modes := terminal.Modes{AlternateScreen: true}
				s.events <- Event{
					Kind:     EventRepaint,
					Data:     []byte(repaintBytes(modes, watchScreen, 23, 79)),
					Modes:    modes,
					HasModes: true,
				}
			},
		},
		{
			name: "live output of a program drawing after the subscribe",
			send: func(s *fakeStream) { s.feed(altScreenDraw(watchScreen, 23, 79)) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp, s := newTallViewer(t, 80, 10, 24, 80)
			tc.send(s)

			waitForRender(t, tp, 80, 10, "Every 1.0s: uptime")
			got := plainRender(tp, 80, 10)
			assert.Contains(t, got, "load average",
				"the output row under the header must show too:\n%s", got)
			lines := strings.Split(got, "\n")
			require.Len(t, lines, 10)
			assert.True(t, strings.HasPrefix(lines[0], "Every 1.0s"),
				"the program's top row must be the view's top row, not cropped off it:\n%s", got)
		})
	}
}

// TestViewerAltScreenFullScreenAppKeepsItsBottomRows pins the other half of the
// alternate-screen rule. A program that fills the screen (htop) also parks its
// cursor on the bottom row, and its last row is content — the key bar. The
// window still ends at the last row holding content, as it did before #7037 and
// as the capture preview does, so committing a preview of htop keeps showing
// the same rows.
func TestViewerAltScreenFullScreenAppKeepsItsBottomRows(t *testing.T) {
	tp, s := newTallViewer(t, 80, 10, 24, 80)
	rows := make([]string, 24)
	for i := range 23 {
		rows[i] = fmt.Sprintf("proc-%02d", i)
	}
	rows[23] = "F1Help  F2Setup F3SearchF4FilterF5Tree"
	s.feed(altScreenDraw(rows, 23, 79) + "\x1b[?25l")

	waitForRender(t, tp, 80, 10, "F1Help")
	got := plainRender(tp, 80, 10)
	assert.Contains(t, got, "proc-14", "a full-screen app's window ends at its last row:\n%s", got)
	assert.NotContains(t, got, "proc-13", "the window is the bottom 10 rows of a full screen:\n%s", got)
}

// TestViewerPrimaryScreenStillAnchorsOnTheCursor pins that the fix is scoped to
// the alternate screen. On the primary screen the cursor is the working edge
// (#4480): it is where a shell's or an agent's next output lands, so the
// window keeps ending on the cursor row even when the rows between it and the
// last output are blank.
func TestViewerPrimaryScreenStillAnchorsOnTheCursor(t *testing.T) {
	tp, s := newTallViewer(t, 80, 10, 24, 80)
	var b strings.Builder
	for i := range 16 {
		fmt.Fprintf(&b, "line-%02d\r\n", i)
	}
	b.WriteString("\r\n\r\n") // the cursor ends three rows below line-15, on row 18
	s.feed(b.String())

	waitForRender(t, tp, 80, 10, "line-15")
	got := plainRender(tp, 80, 10)
	assert.Contains(t, got, "line-09", "the window ends on the cursor row (row 18):\n%s", got)
	assert.NotContains(t, got, "line-08", "the window is the 10 rows ending on the cursor:\n%s", got)
}

// TestMouseClickOnAnAltScreenCropLandsOnThePaintedCell: a click is translated by
// the same window Render shows, so with watch's screen at the top of the view a
// click on view row 2 is emulator row 2 — encoded 1-based as ;3. Translating by
// the old cursor-anchored crop would encode the blank row the click was never
// painted over (;17).
func TestMouseClickOnAnAltScreenCropLandsOnThePaintedCell(t *testing.T) {
	tp, s := newTallViewer(t, 80, 10, 24, 80)
	s.feed(altScreenDraw(watchScreen, 23, 79))
	// Sync on the emulator, not the render, so this test isolates the click
	// translation from the render crop the test above pins.
	require.Eventually(t, func() bool {
		tp.gridMu.RLock()
		defer tp.gridMu.RUnlock()
		c := tp.emu.CellAt(0, 0)
		return tp.emu.IsAltScreen() && c != nil && c.Content == "E" && tp.emu.CursorPosition().Y == 23
	}, 2*time.Second, 5*time.Millisecond, "watch's screen must land on the alternate screen")

	s.feed(sgrMouseModes)
	waitTrackingEnabled(t, tp, true, "DECSET 1002 must enable tracking")

	require.True(t, tp.SendMouse(mouseMsg(tea.MouseActionPress, tea.MouseButtonLeft, 0, 0), 4, 2),
		"a click inside the grid must forward")
	require.Eventually(t, func() bool { return s.sentInput() != "" },
		2*time.Second, 5*time.Millisecond, "the forwarded click never reached the stream")
	assert.Equal(t, "\x1b[<0;5;3M", s.sentInput(),
		"the click must encode the emulator row it was painted over (row 3, 1-based)")
}
