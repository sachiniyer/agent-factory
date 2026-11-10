package session

import (
	"fmt"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"
)

// A held dead pane can never produce output again — pipe-pane refuses it — so
// the ordinary subscribe bring-up fails and, unfixed, the pane renders blank
// (#5330). The broker serves a repaint-only subscription instead: the dead
// pane's screen, delivered through the same pendingRepaint a live pane's
// snapshot uses, and no bytes after — there are none to send.
func TestPTYBrokerDeadPaneServesRepaintOnlyStream(t *testing.T) {
	ch := &fakeClientlessChannel{
		startErr:        fmt.Errorf("pipe-pane: target pane has exited"),
		paneDead:        true,
		snapshot:        []byte("build output tail\nPane is dead"),
		snapshotRows:    24,
		snapshotCols:    80,
		snapshotHasSize: true,
	}
	br := newPTYBroker(ch)

	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe on a dead pane must not fail: %v", err)
	}

	// First the measured-size echo (#4480 ordering), then the repaint.
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}
	if ev.Kind != PTYResize || ev.Rows != 24 || ev.Cols != 80 {
		t.Fatalf("first event = %+v, want the dead pane's measured 24x80 size echo", ev)
	}
	ev, err = nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("repaint NextEvent: %v", err)
	}
	want := "\x1b[2J\x1b[1;1H\x1b[Kbuild output tail\x1b[2;1H\x1b[KPane is dead"
	if ev.Kind != PTYRepaint || string(ev.Data) != want {
		t.Fatalf("second event = %+v, want the dead-pane repaint %q", ev, want)
	}

	// No capture was ever started — there is nothing to capture — and nothing
	// else arrives: the stream is a repaint plus silence.
	if starts, _ := ch.counts(); starts != 0 {
		t.Fatalf("a dead pane must not start a pipe-pane capture; starts = %d", starts)
	}
	if _, err := nextWithin(t, sub, 150*time.Millisecond); err == nil {
		t.Fatal("a dead pane's stream must hold no further events")
	}
}

// A dead pane whose screen cannot be captured cannot be repainted either —
// and unlike a live bring-up there is no capture feeding bytes the subscribe
// could fall back on. Keeping that subscription would park a blank pane
// forever, so the subscribe fails and the caller can retry.
func TestPTYBrokerDeadPaneSnapshotFailureFailsSubscribe(t *testing.T) {
	ch := &fakeClientlessChannel{
		startErr:        fmt.Errorf("pipe-pane: target pane has exited"),
		paneDead:        true,
		snapshotErr:     fmt.Errorf("capture-pane buffer: tmux timeout"),
		snapshotHasSize: true,
	}
	br := newPTYBroker(ch)
	if _, err := br.subscribe(0); err == nil {
		t.Fatal("a dead pane with no capturable screen must not subscribe")
	}
}

// A pane that dies mid-watch — its capture EOFs under the broker — gets the
// same dead-screen repaint rather than an endless re-dial: the re-dial loop
// runs the guarded recovery once, releasing the attached subscribers onto the
// dead pane's screen, and then stops driving.
func TestPTYBrokerRecoveryReleasesSubscribersOntoTheDeadScreen(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("live screen")}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := nextWithin(t, sub, 2*time.Second); err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}

	// The pane dies: its pipe-pane capture can never restart, and the pane's
	// screen is now the dead tail. Closing the fake's writer drops the capture
	// upstream the way a dead tmux pane's pipe does — the readLoop EOFs and
	// hands the recovery to the re-dial loop.
	ch.mu.Lock()
	ch.startErr = fmt.Errorf("pipe-pane: target pane has exited")
	ch.paneDead = true
	ch.snapshot = []byte("command output\nPane is dead")
	ch.mu.Unlock()
	if err := ch.StopCapture(); err != nil {
		t.Fatalf("StopCapture: %v", err)
	}

	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("recovery NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint || !strings.Contains(string(ev.Data), "Pane is dead") {
		t.Fatalf("recovery event = %+v, want the dead-screen repaint", ev)
	}
	// The dead pane was never re-dialed.
	if starts, _ := ch.counts(); starts != 1 {
		t.Fatalf("recovery on a dead pane must not re-dial; starts = %d", starts)
	}
}

// drawnMarker is the row tmux 3.4 draws on a held dead pane (remain-on-exit-format).
const drawnMarker = "Pane is dead (status 0, Tue Nov 10 05:17:42 2026)"

// tmuxDeadBuffer builds `capture-pane -p -e -S -` of a held dead pane exactly as
// tmux returns it (measured on tmux 3.4). capture-pane keeps the visible grid's
// blank rows, and when tmux draws the marker it first scrolls the grid up one
// row (server_destroy_pane: cursor to the last row, linefeed, draw), pushing
// the top row into history. A command that printed `out` and exited on a
// paneH-row pane therefore leaves its output, blank rows, then the marker:
// paneH+1 rows while the output fits on the screen.
func tmuxDeadBuffer(out []string, paneH int) string {
	rows := append([]string(nil), out...)
	// The pre-death screen: the output, then blank rows down to the bottom.
	// Output longer than the pane already scrolled, so there is no blank tail.
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	if len(out) >= paneH {
		rows = append(rows, "") // the cursor's row below the last output line
	}
	// The draw: scroll the screen up one row (the top row stays in the buffer as
	// history), then the marker on the last row.
	rows = append(rows, drawnMarker)
	return strings.Join(rows, "\n") + "\n"
}

func deadScreenRows(capture string, paneH int) []string {
	return strings.Split(strings.TrimSuffix(string(deadPaneScreen(capture, paneH)), "\n"), "\n")
}

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", i+1)
	}
	return out
}

// deadPaneScreen shapes a held dead pane's buffer into the screen a subscriber
// paints (#5330): the command's last output with the "Pane is dead" marker
// directly beneath it, at most a pane tall, whether tmux drew the marker or not.
//
// Directly beneath is the #7061 property. A pane on a dead tab can be any
// height — a viewer crops the tab's grid into a shorter box (#4480), a taller
// box shows all of it — and it must show the output AND the marker either way.
// Only adjacent rows survive every crop. Before #7061 the screen kept the blank
// rows between them, so a one-line command on an 80x24 tab put its output 23
// rows above the marker: tmux's own scroll pushed it off the 24-row tail
// entirely, and a crop that did keep it could not keep the marker too.
func TestDeadPaneScreen(t *testing.T) {
	t.Run("one line that exited, as tmux draws it (#7061)", func(t *testing.T) {
		// The play-test repro: `./test.sh` prints ok and exits on an 80x24 tab.
		buf := tmuxDeadBuffer([]string{"ok"}, 24)
		if n := strings.Count(buf, "\n"); n != 25 {
			t.Fatalf("fixture: tmux leaves 25 rows (ok in history, 23 blank, marker), built %d", n)
		}
		want := []string{"ok", drawnMarker}
		if got := deadScreenRows(buf, 24); !slicesEqual(got, want) {
			t.Fatalf("the output must sit directly above the marker:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("a few lines that exited keep every line", func(t *testing.T) {
		// Measured on a 6-row pane: l1 l2 l3 "" "" "" marker — l1 is history.
		buf := tmuxDeadBuffer([]string{"l1", "l2", "l3"}, 6)
		want := []string{"l1", "l2", "l3", drawnMarker}
		if got := deadScreenRows(buf, 6); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("exactly one screen shows its tail plus the marker", func(t *testing.T) {
		buf := tmuxDeadBuffer(numbered(4), 4)
		want := []string{"line 2", "line 3", "line 4", drawnMarker}
		if got := deadScreenRows(buf, 4); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("longer than the pane shows the tail", func(t *testing.T) {
		buf := tmuxDeadBuffer(numbered(10), 4)
		want := []string{"line 8", "line 9", "line 10", drawnMarker}
		if got := deadScreenRows(buf, 4); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("blank rows inside the output are kept", func(t *testing.T) {
		buf := tmuxDeadBuffer([]string{"build", "", "PASS"}, 8)
		want := []string{"build", "", "PASS", drawnMarker}
		if got := deadScreenRows(buf, 8); !slicesEqual(got, want) {
			t.Fatalf("only the blank rows below the output go; got %q, want %q", got, want)
		}
	})

	t.Run("an undrawn marker is synthesized under the output", func(t *testing.T) {
		// The lost-SIGCHLD shape (#4682): tmux never collected the pane, so the
		// buffer is the raw pre-death screen, blank rows and all.
		buf := "line 1\nline 2\nline 3\n\n\n"
		want := []string{"line 1", "line 2", "line 3", paneDeadMarker}
		if got := deadScreenRows(buf, 5); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("an undrawn marker on a full screen takes the last row", func(t *testing.T) {
		want := []string{"line 2", "line 3", "line 4", paneDeadMarker}
		if got := deadScreenRows("line 1\nline 2\nline 3\nline 4\n", 4); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("output that merely mentions the marker still gets one", func(t *testing.T) {
		// tmux draws its marker on the last row and nothing follows it, so a row
		// of output above other output is not the marker.
		buf := "Pane is dead, said the test\nexit 0\n\n\n"
		want := []string{"Pane is dead, said the test", "exit 0", paneDeadMarker}
		if got := deadScreenRows(buf, 5); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("a drawn marker is kept, not doubled", func(t *testing.T) {
		got := string(deadPaneScreen(tmuxDeadBuffer([]string{"line 2", "line 3"}, 5), 5))
		if strings.Count(xansi.Strip(got), "Pane is dead") != 1 {
			t.Fatalf("a drawn marker must not be duplicated: %q", got)
		}
	})

	t.Run("escapes in the capture survive the slice", func(t *testing.T) {
		// capture-pane -e closes each row's styles at the row's end (measured),
		// so dropping the blank rows below cannot carry a style onto the marker.
		buf := tmuxDeadBuffer([]string{"\x1b[31mred output\x1b[39m"}, 3)
		want := []string{"\x1b[31mred output\x1b[39m", drawnMarker}
		if got := deadScreenRows(buf, 3); !slicesEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
