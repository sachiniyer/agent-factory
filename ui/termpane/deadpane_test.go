package termpane

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/terminal"
)

// deadMarker is the row tmux draws on a held dead pane.
const deadMarker = "Pane is dead (status 0, Tue Nov 10 05:17:42 2026)"

// deadRepaintBytes builds the repaint the daemon sends for a held dead pane
// (session/ptychannel_tmux.go tmuxPaneSnapshot → buildRepaint): the all-off
// primary-screen modes, a clear, each row pinned at its own line, and the
// cursor hidden as tmux reports it — with no cursor restore, since a dead pane
// has no live cursor, so the emulator's cursor stays where the last row ended.
func deadRepaintBytes(rows []string) []byte {
	var b strings.Builder
	b.Write(terminal.Modes{}.RestoreSequence())
	b.WriteString("\x1b[2J")
	for i, row := range rows {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K%s", i+1, row)
	}
	b.WriteString("\x1b[?25l")
	return []byte(b.String())
}

// TestViewerShowsADeadTabsOutputAndMarker pins the TUI half of #7061. The
// daemon's dead-pane screen puts the command's last output directly above the
// "Pane is dead" marker (session's TestDeadPaneScreen pins that half). A pane
// bound to the dead tab is a viewer of its real 80x24 grid, cropped into a box
// of any height (#4480), and it must show both rows as soon as the repaint
// lands — no scroll, resize or keystroke — whether the box is shorter than the
// tab, exactly as tall, or taller.
func TestViewerShowsADeadTabsOutputAndMarker(t *testing.T) {
	long := make([]string, 0, 24)
	for i := 1; i <= 23; i++ {
		long = append(long, fmt.Sprintf("line %02d", i))
	}
	cases := []struct {
		name   string
		screen []string
		want   []string
	}{
		{name: "one line of output", screen: []string{"ok", deadMarker}, want: []string{"ok"}},
		{name: "a full screen of output", screen: append(long, deadMarker), want: []string{"line 23"}},
	}
	for _, tc := range cases {
		for _, boxH := range []int{2, 10, 23, 24, 40} {
			t.Run(fmt.Sprintf("%s/box %d rows", tc.name, boxH), func(t *testing.T) {
				tp, s := newTallViewer(t, 80, boxH, 24, 80)
				s.events <- Event{Kind: EventRepaint, Data: deadRepaintBytes(tc.screen), HasModes: true}

				waitForRender(t, tp, 80, boxH, "Pane is dead")
				got := plainRender(tp, 80, boxH)
				for _, w := range tc.want {
					assert.Contains(t, got, w, "the dead tab's last output must show with its marker:\n%s", got)
				}
			})
		}
	}
}
