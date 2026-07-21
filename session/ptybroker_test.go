package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/sachiniyer/agent-factory/terminal"
)

// fakeClientlessChannel is an in-memory clientlessChannel: StartCapture hands
// back an io.Pipe the test feeds with emit(), and SendRaw/Resize record calls so
// the broker's input/size fan-out can be asserted without a real tmux server.
type fakeClientlessChannel struct {
	mu       sync.Mutex
	r        *io.PipeReader
	w        *io.PipeWriter
	starts   int
	stops    int
	sent     [][]byte
	resizes  [][2]uint16
	startErr error
	// snapshot is the canned current-screen the broker injects as the repaint on
	// subscribe and after a resize; snapshots counts how many times it was read.
	snapshot  []byte
	snapshots int
	// cursorRow/cursorCol/hasCursor are the canned cursor position Snapshot reports
	// (0-based); hasCursor false (the default) means "channel can't report a cursor",
	// so the repaint omits cursor restore — the pre-fix behavior.
	cursorRow int
	cursorCol int
	hasCursor bool
	modes     terminal.Modes
	hasModes  bool
	// wClosed reports whether the CURRENT capture writer (f.w) has been closed by a
	// StopCapture — the test proxy for "the pane pipe was disabled". Reset on each
	// StartCapture. Used by the #1661 teardown-clobber regression test.
	wClosed bool
	// stopEntered/stopRelease gate StopCapture: when both are non-nil, StopCapture
	// signals stopEntered on entry and then blocks on stopRelease before doing any
	// teardown, so a test can pin the "teardown runs mid-reconnect" interleaving.
	stopEntered chan struct{}
	stopRelease chan struct{}
	// stopDone, when non-nil, is signaled AFTER StopCapture has closed the writer,
	// so a test can order an assertion strictly after the teardown's effect.
	stopDone chan struct{}
	// snapshotHook, when non-nil, runs at the START of each Snapshot with f.mu NOT
	// held. The real Snapshot is a `tmux capture-pane` exec taking milliseconds, and
	// the pane keeps producing the whole time; the hook is how a test drives output
	// into that window (the #1975 recovery-flicker race).
	snapshotHook func()
}

func (f *fakeClientlessChannel) StartCapture() (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.starts++
	f.r, f.w = io.Pipe()
	f.wClosed = false
	return f.r, nil
}

func (f *fakeClientlessChannel) StopCapture() error {
	// Gate BEFORE taking f.mu so a concurrent StartCapture can proceed while this
	// teardown is parked — the interleaving the #1661 regression test forces.
	if f.stopEntered != nil && f.stopRelease != nil {
		f.stopEntered <- struct{}{}
		<-f.stopRelease
	}
	f.mu.Lock()
	f.stops++
	if f.w != nil {
		_ = f.w.Close()
		f.wClosed = true
	}
	f.mu.Unlock()
	if f.stopDone != nil {
		f.stopDone <- struct{}{}
	}
	return nil
}

func (f *fakeClientlessChannel) SendRaw(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), b...))
	return nil
}

func (f *fakeClientlessChannel) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint16{rows, cols})
	return nil
}

func (f *fakeClientlessChannel) Snapshot() (PaneSnapshot, error) {
	f.mu.Lock()
	hook := f.snapshotHook
	f.mu.Unlock()
	if hook != nil {
		hook() // runs WITHOUT f.mu so it can emit() into the live capture pipe
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots++
	return PaneSnapshot{
		Screen:    append([]byte(nil), f.snapshot...),
		CursorRow: f.cursorRow,
		CursorCol: f.cursorCol,
		HasCursor: f.hasCursor,
		Modes:     f.modes,
		HasModes:  f.hasModes,
	}, nil
}

// emit writes pane output into the capture pipe. Write blocks until the broker's
// read loop consumes it, so on return the bytes are in flight to the ring.
func (f *fakeClientlessChannel) emit(t *testing.T, b []byte) {
	t.Helper()
	f.mu.Lock()
	w := f.w
	f.mu.Unlock()
	if w == nil {
		t.Fatal("emit before StartCapture")
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

// emitErr writes pane output into the current capture pipe and RETURNS any error
// (rather than fataling) so a test can assert the pipe is still live — a torn-down
// pipe surfaces as a write error.
func (f *fakeClientlessChannel) emitErr(b []byte) error {
	f.mu.Lock()
	w := f.w
	f.mu.Unlock()
	if w == nil {
		return io.ErrClosedPipe
	}
	_, err := w.Write(b)
	return err
}

func nextWithin(t *testing.T, sub PTYSubscription, d time.Duration) (PTYEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return sub.NextEvent(ctx)
}

// mustData blocks for the next event and asserts it is output bytes equal to want.
func mustData(t *testing.T, sub PTYSubscription, want string) {
	t.Helper()
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYData || string(ev.Data) != want {
		t.Fatalf("event = %+v, want data %q", ev, want)
	}
}

// TestPTYBrokerInitialRepaint pins the #1592 PR6 initial-repaint injection: a fresh
// subscriber's FIRST event is a clean repaint of the current screen (pipe-pane
// carries no history, so without it a just-opened pane is blank), delivered as a
// PTYRepaint so the client renders it WITHOUT advancing its replay cursor. A resize
// deliberately injects NO repaint (it would race the SIGWINCH redraw), and a
// reconnecting subscriber (since > 0) gets no repaint either — it resumes via replay.
func TestPTYBrokerInitialRepaint(t *testing.T) {
	ch := &fakeClientlessChannel{snapshot: []byte("SCREEN-A\nline2")}
	br := newPTYBroker(ch)

	sub, err := br.subscribe(0) // fresh live-tail subscriber
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// First event is the initial repaint: ED2, then each captured row placed at its own
	// absolute line (CSI row;1 H + erase-to-EOL + content) so the layout is pinned to
	// the pane grid regardless of the client's width (#1688). It is a PTYRepaint (not
	// counted toward the cursor).
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("initial NextEvent: %v", err)
	}
	want := "\x1b[2J\x1b[1;1H\x1b[KSCREEN-A\x1b[2;1H\x1b[Kline2"
	if ev.Kind != PTYRepaint || string(ev.Data) != want {
		t.Fatalf("initial event = %+v, want PTYRepaint %q", ev, want)
	}

	// A resize broadcasts the authoritative echo but NO repaint (the process redraws
	// itself on SIGWINCH through the stream; a repaint here would race it).
	if err := br.resize(40, 100); err != nil {
		t.Fatalf("resize: %v", err)
	}
	ev, err = nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("resize NextEvent: %v", err)
	}
	if ev.Kind != PTYResize || ev.Rows != 40 || ev.Cols != 100 {
		t.Fatalf("first post-resize event = %+v, want the authoritative resize echo", ev)
	}
	// The next event is live output, NOT a resize repaint.
	ch.emit(t, []byte("live-after-resize"))
	mustData(t, sub, "live-after-resize")

	// A reconnecting subscriber (since > 0) gets NO repaint — it resumes seamlessly
	// via replay. A since past the head clamps to the live tail.
	re, err := br.subscribe(1 << 40)
	if err != nil {
		t.Fatalf("reconnect subscribe: %v", err)
	}
	ch.emit(t, []byte("live"))
	mustData(t, re, "live")
}

func TestPTYBrokerRepaintCarriesTerminalModes(t *testing.T) {
	modes := terminal.Modes{
		AlternateScreen: true,
		MouseTracking:   true,
		MouseButton:     true,
		MouseSGR:        true,
	}
	ch := &fakeClientlessChannel{
		snapshot: []byte("ALT-SCREEN"),
		modes:    modes,
		hasModes: true,
	}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint || !ev.HasModes || ev.Modes != modes {
		t.Fatalf("repaint metadata = %+v, want modes %+v", ev, modes)
	}
	if ev.RepaintProvenance != PTYRepaintFresh {
		t.Fatalf("initial repaint provenance = %d, want fresh", ev.RepaintProvenance)
	}
	if !strings.HasPrefix(string(ev.Data), string(modes.RestoreSequence())) {
		t.Fatalf("repaint data = %q, want terminal-mode restore prefix %q", ev.Data, modes.RestoreSequence())
	}
}

func TestPTYBrokerBlankRepaintStillCarriesTerminalModes(t *testing.T) {
	// An all-false mode value is authoritative HostHistory, not missing data.
	// A fresh pane is commonly blank before its first prompt; suppressing this
	// repaint would leave that client ownership-unknown indefinitely.
	ch := &fakeClientlessChannel{hasModes: true}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint || !ev.HasModes || ev.Modes != (terminal.Modes{}) {
		t.Fatalf("blank repaint = %+v, want authoritative all-false modes", ev)
	}
}

// gridRows renders a vt emulator's grid to plain-text rows (cell contents, blanks
// as spaces) so a test can assert what a terminal would actually show.
func gridRows(emu *vt.Emulator, w, h int) []string {
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			if c := emu.CellAt(x, y); c != nil && c.Content != "" {
				b.WriteString(c.Content)
			} else {
				b.WriteString(" ")
			}
		}
		rows[y] = strings.TrimRight(b.String(), " ")
	}
	return rows
}

// TestPTYBrokerRepaintRestoresCursor pins the duplicated-prompt fix. The initial
// repaint replays a capture-pane snapshot — for a fresh shell that is the prompt on
// row 0 followed by trailing blank rows (capture-pane emits the full pane height).
// Writing that body leaves the emulator cursor at the BOTTOM, but the pane program's
// cursor is really on row 0. Before the fix, the pane's next relative-positioned
// redraw (a shell's SIGWINCH prompt redraw, which uses CR to return to the current
// line) therefore landed at the bottom and orphaned the row-0 copy: the prompt
// rendered TWICE. The fix makes the repaint restore the cursor to the pane's real
// position, so the redraw overwrites in place and the prompt renders ONCE. This is
// the deterministic form of the visual artifact: feed the repaint the broker
// produces, then the CR-redraw, into the SAME vt emulator the TUI/attach consumers
// use, and count the prompt rows.
func TestPTYBrokerRepaintRestoresCursor(t *testing.T) {
	const cols, rows = 40, 8
	const prompt = "user@host$ "
	// The snapshot: prompt on row 0, then trailing blank rows to fill the pane — the
	// exact shape `capture-pane -p -e -J` returns for a just-started shell.
	screen := prompt + strings.Repeat("\n", rows-1)

	// The CR-based prompt redraw a shell emits on the connect-time SIGWINCH: return
	// to column 0 of the CURRENT line, clear it, reprint the prompt.
	redraw := []byte("\r\x1b[K" + prompt)

	repaintFor := func(hasCursor bool) []byte {
		ch := &fakeClientlessChannel{
			snapshot:  []byte(screen),
			hasCursor: hasCursor,
			cursorRow: 0, cursorCol: len(prompt),
		}
		br := newPTYBroker(ch)
		sub, err := br.subscribe(0)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		ev, err := nextWithin(t, sub, 2*time.Second)
		if err != nil {
			t.Fatalf("repaint NextEvent: %v", err)
		}
		if ev.Kind != PTYRepaint {
			t.Fatalf("first event = %+v, want PTYRepaint", ev)
		}
		return ev.Data
	}
	countPromptRows := func(repaint []byte) (int, []string) {
		emu := vt.NewEmulator(cols, rows)
		_, _ = emu.Write(repaint)
		_, _ = emu.Write(redraw)
		grid := gridRows(emu, cols, rows)
		n := 0
		for _, ln := range grid {
			if strings.Contains(ln, "user@host$") {
				n++
			}
		}
		return n, grid
	}

	// With the cursor restored (the fix), the redraw overwrites row 0 — one prompt.
	n, grid := countPromptRows(repaintFor(true))
	if n != 1 {
		t.Fatalf("with cursor restore: prompt on %d rows, want 1 (duplicated-prompt artifact): %#v", n, grid)
	}
	if !strings.Contains(grid[0], "user@host$") {
		t.Fatalf("with cursor restore: prompt not on row 0 (the pane's real cursor row): %#v", grid)
	}

	// Control: WITHOUT the cursor position the pre-fix behavior returns — the redraw
	// lands at the bottom while row 0 stays stale, so the prompt doubles. This proves
	// the test actually discriminates the artifact (and guards the graceful-degrade
	// path the remote channel relies on).
	n, grid = countPromptRows(repaintFor(false))
	if n != 2 {
		t.Fatalf("without cursor: prompt on %d rows, want 2 (the pre-fix doubling this test guards against): %#v", n, grid)
	}
}

// TestPTYBrokerRepaintCursorWidthMismatch is the #1688 gate: the repaint must land
// the restored cursor on the pane's REAL row/col even when the client emulator's
// width differs from the pane's — the case #1676's test (which used client width ==
// pane width) missed.
//
// The bug: the pre-fix repaint wrote the captured screen as one CRLF-joined blob and
// let the client emulator RE-WRAP it by the emulator's own width, then issued an
// ABSOLUTE cursor move to the pane's cursor_y. When the emulator width != pane width
// the re-wrap shifts the rows, so the absolute row no longer names the row the
// content actually landed on — the cursor sits on the wrong line, and Claude's
// relative-cursor status-block redraw corrupts the whole frame.
//
// The scenario models a pane of width paneW with the real cursor on the "PROMPT>"
// row, and renders the repaint into an emulator NARROWER than the pane (clientW <
// paneW) — the general width-mismatch case. The invariant asserted is
// correct-by-construction: the emulator row the pane's cursor names must show the
// content that was on that pane row, and the emulator cursor must equal the pane
// cursor exactly.
func TestPTYBrokerRepaintCursorWidthMismatch(t *testing.T) {
	const paneW, paneH = 20, 6
	// The pane grid, one entry per physical pane row — exactly what a grid-form
	// capture (capture-pane WITHOUT -J) returns, trailing blank rows stripped. Row 0
	// is full pane width (a command that wrapped), row 1 its continuation; the live
	// prompt is on row 3, where the pane program's cursor sits.
	gridRowsIn := []string{
		"0123456789ABCDEFGHIJ", // row 0: full width (20)
		"KLMNO",                // row 1: wrap continuation of row 0's logical line
		"file1",                // row 2
		"PROMPT>",              // row 3: the pane program's cursor row
	}
	const curRow, curCol = 3, 7 // real pane cursor: end of "PROMPT>"
	screen := strings.Join(gridRowsIn, "\n")

	// Assert the invariant at BOTH a narrower and a wider client than the pane. The
	// narrow case is the one the pre-fix CRLF-join repaint got wrong: at clientW <
	// paneW row 0 re-wraps into two emulator rows, shifting "PROMPT>" down while the
	// absolute cursor move still names row 3 — landing the cursor a row above the
	// prompt.
	for _, clientW := range []int{10, 40} {
		t.Run(fmt.Sprintf("clientW=%d", clientW), func(t *testing.T) {
			ch := &fakeClientlessChannel{
				snapshot:  []byte(screen),
				hasCursor: true,
				cursorRow: curRow, cursorCol: curCol,
			}
			br := newPTYBroker(ch)
			sub, err := br.subscribe(0)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			ev, err := nextWithin(t, sub, 2*time.Second)
			if err != nil {
				t.Fatalf("repaint NextEvent: %v", err)
			}
			if ev.Kind != PTYRepaint {
				t.Fatalf("first event = %+v, want PTYRepaint", ev)
			}

			emu := vt.NewEmulator(clientW, paneH)
			if _, err := emu.Write(ev.Data); err != nil {
				t.Fatalf("emulator write: %v", err)
			}

			// Invariant 1: the emulator cursor lands exactly on the pane's real cursor.
			pos := emu.CursorPosition()
			if pos.Y != curRow || pos.X != curCol {
				t.Fatalf("post-repaint cursor = (row %d,col %d), want the pane's real (row %d,col %d) at clientW=%d != paneW=%d",
					pos.Y, pos.X, curRow, curCol, clientW, paneW)
			}
			// Invariant 2: the row the cursor names shows that pane row's content (the
			// prompt), not another row shifted in by the emulator's re-wrap.
			grid := gridRows(emu, clientW, paneH)
			if !strings.Contains(grid[curRow], "PROMPT>") {
				t.Fatalf("emulator row %d = %q, want the pane's %q; rows=%#v", curRow, grid[curRow], "PROMPT>", grid)
			}
		})
	}
}

// TestPTYBrokerRepaintTrailingNewlineKeepsBottomRow guards the trailing-newline
// edge Greptile/T-Rex reproduced. Real `capture-pane -p -e` (no -J) ends with ONE
// trailing "\n" after the last row (it strips trailing blank rows, so that "\n" is a
// row SEPARATOR, not a real empty row). If buildRepaint splits without trimming it,
// the split yields a phantom trailing "" element and emits an out-of-range
// CSI (N+1);1 H + erase — which, in an emulator clamped to the pane height, clamps
// onto the real bottom row and WIPES it (often Claude's input/status line). The fake
// emulator runs WITHOUT tmux, so this closes the CI coverage gap the real-tmux test
// left (tmux is not on the CI PATH, so that test is skipped there).
func TestPTYBrokerRepaintTrailingNewlineKeepsBottomRow(t *testing.T) {
	const cols, rows = 20, 3
	// A snapshot that FILLS the pane height (all 3 rows have content) and ends with a
	// trailing "\n" — exactly what capture-pane emits. The bottom row is the one the
	// phantom out-of-range clear would wipe.
	screen := "top-row\nmid-row\nBOTTOM-ROW\n"
	const curRow, curCol = 2, 10 // cursor on the bottom row (end of "BOTTOM-ROW")

	ch := &fakeClientlessChannel{
		snapshot:  []byte(screen),
		hasCursor: true,
		cursorRow: curRow, cursorCol: curCol,
	}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ev, err := nextWithin(t, sub, 2*time.Second)
	if err != nil {
		t.Fatalf("repaint NextEvent: %v", err)
	}
	if ev.Kind != PTYRepaint {
		t.Fatalf("first event = %+v, want PTYRepaint", ev)
	}

	emu := vt.NewEmulator(cols, rows)
	if _, err := emu.Write(ev.Data); err != nil {
		t.Fatalf("emulator write: %v", err)
	}
	grid := gridRows(emu, cols, rows)
	if !strings.Contains(grid[curRow], "BOTTOM-ROW") {
		t.Fatalf("bottom row erased by phantom trailing-newline clear: row %d = %q; rows=%#v", curRow, grid[curRow], grid)
	}
	// And the cursor is still on the bottom row where the pane program left it.
	if pos := emu.CursorPosition(); pos.Y != curRow || pos.X != curCol {
		t.Fatalf("post-repaint cursor = (row %d,col %d), want (row %d,col %d)", pos.Y, pos.X, curRow, curCol)
	}
}

func TestPTYBrokerFanout(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	b, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe b: %v", err)
	}
	ch.emit(t, []byte("hello"))
	mustData(t, a, "hello")
	mustData(t, b, "hello")
	if ch.starts != 1 {
		t.Fatalf("StartCapture calls = %d, want 1 (lazy, once)", ch.starts)
	}
}

func TestPTYBrokerReplayFromSeq(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	ch.emit(t, []byte("hello"))
	mustData(t, a, "hello")
	if got := a.Seq(); got != 5 {
		t.Fatalf("seq after 5 bytes = %d, want 5", got)
	}
	// A reconnecting client resumes from a prior cursor and replays the gap.
	b, err := br.subscribe(2)
	if err != nil {
		t.Fatalf("subscribe replay: %v", err)
	}
	mustData(t, b, "llo")
}

func TestPTYBrokerSinceZeroIsLiveTail(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	ch.emit(t, []byte("old"))
	mustData(t, a, "old")
	// since==0 means "from the live tail": no replay of already-buffered bytes.
	b, _ := br.subscribe(0)
	ch.emit(t, []byte("new"))
	mustData(t, b, "new")
}

func TestPTYBrokerResizeEchoBroadcast(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	b, _ := br.subscribe(0)
	if err := br.resize(24, 80); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// last-resize-wins: a second resize supersedes the first authoritative size.
	if err := br.resize(30, 100); err != nil {
		t.Fatalf("resize 2: %v", err)
	}
	for _, sub := range []PTYSubscription{a, b} {
		ev, err := nextWithin(t, sub, 2*time.Second)
		if err != nil {
			t.Fatalf("resize NextEvent: %v", err)
		}
		if ev.Kind != PTYResize || ev.Rows != 30 || ev.Cols != 100 {
			t.Fatalf("resize echo = %+v, want 30x100 (last-wins)", ev)
		}
	}
	if len(ch.resizes) != 2 {
		t.Fatalf("channel resizes = %v, want 2 applied", ch.resizes)
	}
}

func TestPTYBrokerInput(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	if err := br.input([]byte{0x1b, 0x5b, 0x41}); err != nil { // up arrow
		t.Fatalf("input: %v", err)
	}
	if len(ch.sent) != 1 || string(ch.sent[0]) != "\x1b[A" {
		t.Fatalf("sent = %v, want the up-arrow bytes", ch.sent)
	}
}

func TestPTYBrokerCloseEOF(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	br.close()
	if _, err := nextWithin(t, a, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("NextEvent after close = %v, want io.EOF", err)
	}
}

func TestPTYBrokerEvictionFastForwards(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	br.maxBytes = 4 // tiny ring
	// Subscribe but do not read, then overflow the ring so its tail is evicted.
	b, err := br.subscribe(1) // a stale cursor into the soon-evicted region
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ch.emit(t, []byte("abcdefgh"))
	// The eviction fast-forwarded this cursor over the lost gap — a jump the SERVER
	// made, which the client (counting the bytes it received) cannot see. So the gap is
	// announced as a cursor re-seed BEFORE the retained tail, rather than silently
	// (#1845 follow-up): a client that kept counting from its stale cursor would
	// reconnect with a ?since below base and be re-sent bytes it already rendered.
	ev, err := nextWithin(t, b, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent (want the cursor re-seed): %v", err)
	}
	if ev.Kind != PTYCursor || ev.Seq != 4 {
		t.Fatalf("event = %+v, want PTYCursor Seq=4 (the oldest retained byte)", ev)
	}
	// Then the retained tail: the cursor resumes at the oldest retained byte rather
	// than replaying evicted data.
	ev, err = nextWithin(t, b, 2*time.Second)
	if err != nil {
		t.Fatalf("NextEvent: %v", err)
	}
	if ev.Kind != PTYData || string(ev.Data) != "efgh" {
		t.Fatalf("event = %+v, want the retained tail %q", ev, "efgh")
	}
}

// TestPTYBrokerTeardownDoesNotClobberReconnect is the #1661 regression: the
// clientless channel drives a SINGLE pane pipe, so the last-subscriber teardown
// (StopCapture → `pipe-pane`-disable) must not run AFTER a new subscriber has
// brought the capture back up — that ordering would disable the fresh pipe and
// leave the reconnected subscriber (e.g. an embedded pane re-bound after a
// full-screen attach+detach) with a live ring but no bytes ever arriving.
//
// The test pins the hostile interleaving deterministically: subscriber A leaves
// and its teardown parks inside StopCapture; subscriber B reconnects while the
// teardown is mid-flight; then the teardown is released. B must end up on a LIVE
// capture and receive freshly emitted output.
func TestPTYBrokerTeardownDoesNotClobberReconnect(t *testing.T) {
	ch := &fakeClientlessChannel{
		stopEntered: make(chan struct{}, 1),
		stopRelease: make(chan struct{}),
		stopDone:    make(chan struct{}, 1),
	}
	br := newPTYBroker(ch)

	a, err := br.subscribe(0) // brings the capture up (StartCapture #1)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}

	// A leaves: its last-subscriber teardown parks inside the gated StopCapture.
	go func() { _ = a.Close() }()
	select {
	case <-ch.stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never reached StopCapture")
	}

	// B reconnects while the teardown is parked. With the fix this blocks on the
	// capture reconcile until the teardown finishes and then starts a FRESH
	// capture; without it, B starts a capture the parked teardown then disables.
	type subResult struct {
		sub *ptySub
		err error
	}
	bres := make(chan subResult, 1)
	go func() {
		s, e := br.subscribe(0)
		bres <- subResult{s, e}
	}()

	// Give B time to reach the capture reconcile (blocked) or, in the buggy path,
	// to start its capture before we release the teardown.
	time.Sleep(50 * time.Millisecond)
	close(ch.stopRelease)

	var b subResult
	select {
	case b = <-bres:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnecting subscribe B never returned")
	}
	if b.err != nil {
		t.Fatalf("subscribe B: %v", b.err)
	}

	// Order the assertion strictly AFTER the parked teardown's writer-close so the
	// emit below cannot race ahead of a clobber and pass spuriously.
	select {
	case <-ch.stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never completed")
	}

	// The reconnected subscriber must be on a LIVE pipe: a freshly emitted byte
	// reaches it. A clobbered pipe surfaces as a write error here (#1661).
	if err := ch.emitErr([]byte("LIVE-AFTER-RECONNECT")); err != nil {
		t.Fatalf("capture pipe was torn down under the reconnected subscriber (#1661): %v", err)
	}
	mustData(t, b.sub, "LIVE-AFTER-RECONNECT")
}

func TestPTYBrokerStopsCaptureWhenLastSubscriberLeaves(t *testing.T) {
	ch := &fakeClientlessChannel{}
	br := newPTYBroker(ch)
	a, _ := br.subscribe(0)
	b, _ := br.subscribe(0)
	_ = a.Close()
	if ch.stops != 0 {
		t.Fatalf("StopCapture after 1 of 2 leaves = %d, want 0 (capture stays up)", ch.stops)
	}
	_ = b.Close()
	if ch.stops != 1 {
		t.Fatalf("StopCapture after last leaves = %d, want 1", ch.stops)
	}
	// A later subscribe restarts the capture (lazy, again).
	if _, err := br.subscribe(0); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	if ch.starts != 2 {
		t.Fatalf("StartCapture calls = %d, want 2 (restarted)", ch.starts)
	}
}
