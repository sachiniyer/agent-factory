package session

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestTmuxSnapshotRepaintCursorRealTmux is the #1688 end-to-end gate against a REAL
// tmux server: it drives the actual capture → buildRepaint → emulator path and
// asserts the restored cursor lands on its true pane row when the client emulator's
// width differs from the pane's.
//
// The pane (20 wide) holds a line that WRAPS (25 chars → two physical rows) above a
// prompt carrying a unique marker where the pane cursor sits. Under the pre-fix
// `capture-pane -J` + CRLF-join repaint, that wrapped line collapses to one row in a
// 40-wide emulator and shifts the marker up, so the absolute cursor move named the
// wrong line. The grid capture (`capture-pane` without -J) + per-row positioning
// makes the layout width-independent, so the emulator row the cursor names still
// shows the marker.
func TestTmuxSnapshotRepaintCursorRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	ts := tmux.NewTmuxSession("repaint-1688", "bash --noprofile --norc -i")
	if err := ts.Start(t.TempDir()); err != nil {
		t.Fatalf("start tmux session: %v", err)
	}
	t.Cleanup(func() { _, _ = ts.Close() })

	// Narrow the pane so a 25-char line wraps across two physical rows.
	const paneW, paneH = 20, 8
	if err := ts.ResizeWindow(paneW, paneH); err != nil {
		t.Fatalf("resize window: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Print a wrapping line, then leave the cursor after a unique marker on the live
	// prompt line (no Enter) so the pane cursor sits at a known row/col.
	if err := ts.SendKeysCommand("printf '%s\\n' AAAAAAAAAAAAAAAAAAAABBBBB"); err != nil {
		t.Fatalf("send wrapping line: %v", err)
	}
	const marker = "ZZMARK"
	if err := ts.SendRawKeys([]byte(marker)); err != nil {
		t.Fatalf("send marker: %v", err)
	}

	ch := newTmuxClientlessChannel(ts)
	var snap PaneSnapshot
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, err := ch.Snapshot()
		if err == nil && s.HasCursor && strings.Contains(string(s.Screen), marker) {
			snap = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker/cursor never appeared in snapshot: screen=%q hasCursor=%v err=%v",
				string(s.Screen), s.HasCursor, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The grid capture must SPLIT the wrapped line into two physical rows (not
	// -J-join it) — the property the cursor mapping depends on.
	if !strings.Contains(string(snap.Screen), "AAAAAAAAAAAAAAAAAAAA\nBBBBB") {
		t.Fatalf("grid capture did not split the wrapped line across rows: %q", string(snap.Screen))
	}
	// The marker must be on the pane's real cursor row, and the cursor just past it.
	gridLines := strings.Split(string(snap.Screen), "\n")
	if snap.CursorRow >= len(gridLines) || !strings.Contains(gridLines[snap.CursorRow], marker) {
		t.Fatalf("pane cursor row %d does not hold the marker; lines=%#v", snap.CursorRow, gridLines)
	}

	// Render the repaint into an emulator WIDER than the pane (the mismatch that broke
	// the -J path), and assert the cursor row still shows the marker.
	const clientW = 40
	emu := vt.NewEmulator(clientW, paneH)
	if _, err := emu.Write(buildRepaint(snap)); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	pos := emu.CursorPosition()
	if pos.Y != snap.CursorRow || pos.X != snap.CursorCol {
		t.Fatalf("post-repaint cursor = (row %d,col %d), want the pane's real (row %d,col %d) at clientW=%d != paneW=%d",
			pos.Y, pos.X, snap.CursorRow, snap.CursorCol, clientW, paneW)
	}
	row := gridRows(emu, clientW, paneH)[snap.CursorRow]
	if !strings.Contains(row, marker) {
		t.Fatalf("emulator cursor row %d = %q, want the marker %q (cursor landed on the wrong line)", snap.CursorRow, row, marker)
	}
}

// TestClientlessCaptureCloseWakesBlockedRead is the #1943 gate: tearing down the
// clientless capture must wake a read loop that is parked inside a blocking
// read(2) on the pipe-pane FIFO. The FIFO is opened O_RDWR and darwin's kqueue
// cannot poll fifos, so the fd never enters the netpoller and closing it does
// not interrupt an in-flight read — before the fix, the teardown join below
// hangs forever, which is exactly how a session delete wedged the daemon
// (KillSession → ptyBroker.close blocks on captureMu behind the stuck join).
func TestClientlessCaptureCloseWakesBlockedRead(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	ts := tmux.NewTmuxSession("fifo-wake-1943", "bash --noprofile --norc -i")
	if err := ts.Start(t.TempDir()); err != nil {
		t.Fatalf("start tmux session: %v", err)
	}
	// Pane state deliberately ignored: this is #1944's FIFO-wake regression test
	// tearing down its own fixture session; nothing destructive follows. The second
	// return is new in this PR (tmux.PaneState) — the production captureReader from
	// #1944 is untouched, only this call site acknowledges the added value.
	t.Cleanup(func() { _, _ = ts.Close() })

	ch := newTmuxClientlessChannel(ts)
	rc, err := ch.StartCapture()
	if err != nil {
		t.Fatalf("start capture: %v", err)
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			if _, err := rc.Read(buf); err != nil {
				return
			}
		}
	}()

	// Let the reader drain the shell's startup output and park inside a
	// blocking Read with the pane idle — the state a session sits in when the
	// user deletes it.
	time.Sleep(300 * time.Millisecond)

	// Mirror ptyBroker's stopCapture teardown: stop the capture, close the
	// reader, then join the read loop. The join must complete promptly.
	if err := ch.StopCapture(); err != nil {
		t.Fatalf("stop capture: %v", err)
	}
	_ = rc.Close()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop still blocked after StopCapture+Close; capture teardown cannot join a parked FIFO read (#1943)")
	}
}
