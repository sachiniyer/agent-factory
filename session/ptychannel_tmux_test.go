package session

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestPipePaneCommandUsesCatOnDarwin(t *testing.T) {
	const fifo = "/tmp/af pane's output"
	got := pipePaneCommandForGOOS(fifo, "darwin")
	want := `cat > '/tmp/af pane'"'"'s output'`
	if got != want {
		t.Fatalf("Darwin pipe-pane command = %q, want %q", got, want)
	}
}

func TestPipePaneCommandKeepsStreamingDDOutsideDarwin(t *testing.T) {
	const fifo = "/tmp/af pane's output"
	got := pipePaneCommandForGOOS(fifo, "linux")
	want := `dd of='/tmp/af pane'"'"'s output' bs=65536 2>/dev/null`
	if got != want {
		t.Fatalf("Linux pipe-pane command = %q, want %q", got, want)
	}
}

type captureReadResult struct {
	buf []byte
	err error
}

func readAcrossCaptureStop(t *testing.T, data []byte) captureReadResult {
	t.Helper()
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe: %v", err)
	}
	r := &captureReader{f: outputR, keepalive: outputW}
	result := make(chan captureReadResult, 1)
	go func() {
		buf, readErr := io.ReadAll(r)
		result <- captureReadResult{buf: buf, err: readErr}
	}()

	if _, err := outputW.Write(data); err != nil {
		t.Fatalf("write final output: %v", err)
	}
	if err := r.stop(true); err != nil {
		t.Fatalf("stop capture reader: %v", err)
	}
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("capture reader did not reach FIFO EOF after graceful stop")
		return captureReadResult{}
	}
}

func TestCaptureReaderPreservesBytesReadAsCloseStarts(t *testing.T) {
	const finalOutput = "final pane output"
	got := readAcrossCaptureStop(t, []byte(finalOutput))
	if string(got.buf) != finalOutput {
		t.Fatalf("Read data = %q (n=%d), want final pane output %q", got.buf, len(got.buf), finalOutput)
	}
	if got.err != nil {
		t.Fatalf("ReadAll error = %v", got.err)
	}
}

func TestCaptureReaderPreservesRealTrailingNULAtStop(t *testing.T) {
	finalOutput := []byte("final pane output\x00")
	got := readAcrossCaptureStop(t, finalOutput)
	if !bytes.Equal(got.buf, finalOutput) {
		t.Fatalf("Read data = %q (n=%d), want raw final pane output %q", got.buf, len(got.buf), finalOutput)
	}
}

func TestCaptureReaderDrainsMoreThanOneBufferBeforeStop(t *testing.T) {
	finalOutput := bytes.Repeat([]byte("pane-output-"), 8*1024)
	got := readAcrossCaptureStop(t, finalOutput)
	if !bytes.Equal(got.buf, finalOutput) {
		t.Fatalf("ReadAll returned %d bytes, want all %d final pane bytes", len(got.buf), len(finalOutput))
	}
}

func TestCaptureReaderAbortWakesWithExternalWriterStillOpen(t *testing.T) {
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe: %v", err)
	}
	externalFD, err := syscall.Dup(int(outputW.Fd()))
	if err != nil {
		_ = outputR.Close()
		_ = outputW.Close()
		t.Fatalf("duplicate external writer: %v", err)
	}
	externalW := os.NewFile(uintptr(externalFD), "capture-external-writer")
	t.Cleanup(func() { _ = externalW.Close() })
	r := &captureReader{f: outputR, keepalive: outputW}
	result := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, readErr := r.Read(buf)
		result <- readErr
	}()

	if err := r.Close(); err != nil {
		t.Fatalf("close capture reader: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Read error = %v, want EOF from failure-only abort wake", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture reader stayed blocked with an external writer open")
	}
}

func TestCaptureReaderCloseAfterAbortCleanup(t *testing.T) {
	outputR, outputW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe: %v", err)
	}
	r := &captureReader{f: outputR, keepalive: outputW}

	// Reproduce the losing stop interleaving deterministically: stop has latched
	// abort, then the reader observes it and finishes cleanup before stop reaches
	// the keeper close.
	r.abort.Store(true)
	buf := make([]byte, 1)
	if _, err := r.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want EOF after abort", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close after reader cleanup = %v, want nil", err)
	}
}

// TestTmuxSnapshotRepaintCursorRealTmux is the #1688 end-to-end gate against a REAL
// tmux server: it drives the actual capture → buildRepaint → emulator path and
// asserts the restored cursor lands on its true pane row when the client emulator's
// width differs from the pane's.
//
// The pane (20 wide) holds a line that WRAPS (25 chars → two physical rows) above a
// final row carrying a unique marker where the pane cursor sits. Under the pre-fix
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

	// Print a wrapping line, then a unique marker with NO trailing newline, as ONE
	// submitted command, so the cursor comes to rest on the marker's row.
	//
	// One command on purpose (#3270): the marker used to be typed via SendRawKeys
	// after this returned, but SendKeysCommand only proves DELIVERY, not that the
	// shell has finished executing — and the tty echoes type-ahead input the moment
	// it arrives, even mid-execution. On a slow runner the marker's echo therefore
	// entered the pane stream BEFORE printf's output, the wait below saw the marker
	// and captured a screen with no output rows on it, and the split assertion
	// failed. Emitted by the command itself, the marker is the LAST thing the pane
	// renders, so its visibility implies every row above it is already there. It is
	// spelled 'ZZ%s' MARK so the echoed command line can never contain the
	// contiguous marker and satisfy the wait early.
	const marker = "ZZMARK"
	if err := ts.SendKeysCommand("printf '%s\\n' AAAAAAAAAAAAAAAAAAAABBBBB; printf 'ZZ%s' MARK"); err != nil {
		t.Fatalf("send wrapping line: %v", err)
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

// TestTmuxSnapshotRepaintCursorVisibilityRealTmux is the end-to-end phantom-cursor
// gate against a REAL tmux server: it drives the actual capture → ReadTerminalState
// → buildRepaint → emulator path and asserts that a pane application that has hidden
// its cursor (DECTCEM off, ?25l) produces a repaint that leaves a fresh emulator's
// cursor hidden — not the default visible cursor the pre-fix repaint left.
//
// The bug: the snapshot/repaint path captured cursor position and ownership modes
// (alternate-screen, mouse) but never cursor visibility, so a fresh subscriber's
// emulator kept its default visible cursor even when the app explicitly hid it.
// Apps that only emit ?25l on transitions (Claude's composer, Codex's pickers, vim
// between operations) left a phantom cursor overlay indefinitely.
func TestTmuxSnapshotRepaintCursorVisibilityRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	ts := tmux.NewTmuxSession("repaint-cursorvis", "bash --noprofile --norc -i")
	if err := ts.Start(t.TempDir()); err != nil {
		t.Fatalf("start tmux session: %v", err)
	}
	t.Cleanup(func() { _, _ = ts.Close() })

	if err := ts.ResizeWindow(40, 8); err != nil {
		t.Fatalf("resize window: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Hide the cursor and print a marker so the snapshot has content.
	if err := ts.SendKeysCommand("printf 'HIDDEN CURSOR\\n\\033[?25l'"); err != nil {
		t.Fatalf("send hide-cursor: %v", err)
	}

	ch := newTmuxClientlessChannel(ts)
	var snap PaneSnapshot
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, err := ch.Snapshot()
		if err == nil && s.HasVisibility && strings.Contains(string(s.Screen), "HIDDEN CURSOR") {
			snap = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never carried visibility/content: screen=%q hasVisibility=%v err=%v",
				string(s.Screen), s.HasVisibility, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The snapshot must report the cursor as hidden — tmux's own cursor_flag.
	if snap.CursorVisible {
		t.Fatalf("snapshot CursorVisible = true, want false — the pane hid the cursor (?25l)")
	}

	// Feed the repaint into a fresh emulator and track DECTCEM transitions.
	// The emulator starts with the cursor visible (the default); the repaint
	// must emit ?25l to flip it to hidden.
	const cols, rows = 40, 8
	emu := vt.NewEmulator(cols, rows)
	var gotVisible bool
	emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { gotVisible = visible },
	})
	if _, err := emu.Write(buildRepaint(snap)); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	if gotVisible {
		t.Fatalf("post-repaint cursor visible = true, want false — the repaint must restore the pane's hidden cursor (?25l), not leave the default visible")
	}

	// Control: a pane with the cursor visible must produce a repaint that
	// keeps the emulator cursor visible. The vt emulator starts with the
	// cursor visible and its CursorVisibility callback fires only on state
	// changes, so pre-hide the cursor before feeding the repaint — ?25h in
	// the repaint must flip it back to visible. This proves the emulator
	// tracks DECTCEM correctly the moment the snapshot path replays it.
	if err := ts.SendKeysCommand("printf '\\033[?25h\\nVISIBLE CURSOR'"); err != nil {
		t.Fatalf("send show-cursor: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		s, err := ch.Snapshot()
		if err == nil && s.HasVisibility && s.CursorVisible && strings.Contains(string(s.Screen), "VISIBLE CURSOR") {
			snap = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("visible-cursor snapshot never appeared: screen=%q hasVisibility=%v visible=%v err=%v",
				string(s.Screen), s.HasVisibility, s.CursorVisible, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	emu2 := vt.NewEmulator(cols, rows)
	var gotVisible2 bool
	emu2.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { gotVisible2 = visible },
	})
	// Pre-hide so the repaint's ?25h is a state change the callback observes.
	if _, err := emu2.Write([]byte("\x1b[?25l")); err != nil {
		t.Fatalf("pre-hide write: %v", err)
	}
	if _, err := emu2.Write(buildRepaint(snap)); err != nil {
		t.Fatalf("emulator write (visible): %v", err)
	}
	if !gotVisible2 {
		t.Fatalf("post-repaint cursor visible = false, want true — the repaint must keep the pane's visible cursor (?25h)")
	}
}

// TestClientlessCaptureCloseWakesBlockedRead is the #1943 gate: tearing down the
// clientless capture must wake a read loop that is parked inside a blocking
// FIFO wait. Darwin's kqueue cannot poll FIFOs through Go's netpoller, and
// close(2) does not interrupt an in-flight FIFO read. Before the original fix,
// the teardown join below hung forever and wedged session delete; the current
// implementation must preserve that wake guarantee while draining to FIFO EOF.
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
	// tearing down its own fixture session; nothing destructive follows.
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
