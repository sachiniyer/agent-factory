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
	t.Cleanup(func() { _ = ts.Close() })

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
