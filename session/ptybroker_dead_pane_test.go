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

// deadPaneScreen bottom-anchors the buffer to the pane height and guarantees
// the "Pane is dead" marker as the last line, whether tmux drew it or not —
// short, exactly-one-screen, and longer captures each show their tail plus the
// marker (#5330).
func TestDeadPaneScreen(t *testing.T) {
	t.Run("short output synthesizes the marker on the last row", func(t *testing.T) {
		// tmux never drew the marker: the grid is the raw pre-death screen.
		var buf strings.Builder
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(&buf, "line %d\n", i)
		}
		got := string(deadPaneScreen(buf.String(), 5))
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		if len(lines) != 5 {
			t.Fatalf("want 5 rows, got %d: %q", len(lines), got)
		}
		if lines[4] != "Pane is dead" {
			t.Fatalf("the marker must sit on the last row, got %q", lines[4])
		}
		if lines[0] != "line 1" || lines[2] != "line 3" {
			t.Fatalf("short output keeps its rows: %q", lines)
		}
	})

	t.Run("exactly one screen keeps its tail plus the marker", func(t *testing.T) {
		// A full screen of output plus the marker is one row over the pane —
		// the top row scrolls off, just as tmux's own draw scrolls the grid.
		var buf strings.Builder
		for i := 1; i <= 4; i++ {
			fmt.Fprintf(&buf, "line %d\n", i)
		}
		got := string(deadPaneScreen(buf.String(), 4))
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		want := []string{"line 2", "line 3", "line 4", "Pane is dead"}
		for i := range want {
			if lines[i] != want[i] {
				t.Fatalf("row %d = %q, want %q in %q", i, lines[i], want[i], lines)
			}
		}
	})

	t.Run("longer than the pane shows the tail", func(t *testing.T) {
		var buf strings.Builder
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&buf, "line %d\n", i)
		}
		got := string(deadPaneScreen(buf.String(), 4))
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("want the last 4 rows, got %d: %q", len(lines), got)
		}
		want := []string{"line 8", "line 9", "line 10", "Pane is dead"}
		for i := range want {
			if lines[i] != want[i] {
				t.Fatalf("row %d = %q, want %q in %q", i, lines[i], want[i], lines)
			}
		}
	})

	t.Run("a drawn marker is kept, not doubled", func(t *testing.T) {
		// tmux already drew its marker (scroll + draw on the last row).
		buf := "line 2\nline 3\n\n\nPane is dead (status 0, Mon 27 Oct 2025 10:00:00)\n"
		got := string(deadPaneScreen(buf, 5))
		if strings.Count(xansi.Strip(got), "Pane is dead") != 1 {
			t.Fatalf("a drawn marker must not be duplicated: %q", got)
		}
		lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
		if !strings.HasPrefix(lines[4], "Pane is dead") {
			t.Fatalf("the drawn marker stays on the last row: %q", lines[4])
		}
	})

	t.Run("escapes in the capture survive the slice", func(t *testing.T) {
		buf := "\x1b[31mred output\x1b[0m\n"
		got := string(deadPaneScreen(buf, 3))
		if !strings.Contains(got, "\x1b[31mred output\x1b[0m") {
			t.Fatalf("ANSI content must pass through untouched: %q", got)
		}
	})
}
