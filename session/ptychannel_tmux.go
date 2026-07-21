package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// tmuxClientlessChannel is the local runtime's clientlessChannel (#1592 Phase 2
// PR5): it captures a tmux pane's output through `pipe-pane` into a private FIFO
// and drives input/size through `send-keys`/`resize-window` — all WITHOUT a
// `tmux attach-session` render client. The WS PTY broker fans the FIFO bytes to
// its subscribers; PR6 deletes the render client the TUI still uses today.
type tmuxClientlessChannel struct {
	ts *tmux.TmuxSession

	mu   sync.Mutex
	dir  string         // temp dir holding the FIFO, removed on StopCapture
	fifo string         // FIFO path pipe-pane writes to
	rc   *captureReader // read end of the FIFO
}

// captureReader wraps the FIFO's read end so Close wakes a Read parked in
// read(2). The FIFO cannot enter Go's netpoller on darwin (kqueue does not
// work with fifos — see os/file_unix.go), so the fd is a plain blocking
// descriptor and close(2) does NOT interrupt an in-flight read: with pipe-pane
// already gone no byte would ever arrive, and the teardown join in
// ptyBroker.stopCapture hung forever, wedging session delete and with it the
// whole daemon (#1943). Close latches stopped, then writes one sentinel byte
// into the FIFO — the O_RDWR read end keeps a reader registered, so the
// nonblocking writer open cannot fail with ENXIO — forcing any parked read to
// return. Read reports io.EOF once stopped, so the sentinel never reaches the
// broker ring.
type captureReader struct {
	f       io.ReadCloser
	fifo    string
	stopped atomic.Bool
	once    sync.Once
	err     error
}

const captureStopSentinel byte = 0

func (r *captureReader) Read(p []byte) (int, error) {
	if r.stopped.Load() {
		return 0, io.EOF
	}
	n, err := r.f.Read(p)
	if r.stopped.Load() {
		// Close writes exactly one sentinel after latching stopped. StopCapture has
		// already disabled pipe-pane, so when that byte shares a FIFO read with the
		// pane's final buffered output it is the tail. Consume only that private wake
		// byte; bytes the read already obtained still belong to the broker ring and
		// must be returned before EOF.
		if n > 0 && p[n-1] == captureStopSentinel {
			n--
		}
		if n > 0 {
			return n, io.EOF
		}
		return 0, io.EOF
	}
	return n, err
}

func (r *captureReader) Close() error {
	r.once.Do(func() {
		r.stopped.Store(true)
		if w, err := os.OpenFile(r.fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_, _ = w.Write([]byte{captureStopSentinel})
			_ = w.Close()
		}
		r.err = r.f.Close()
	})
	return r.err
}

var _ clientlessChannel = (*tmuxClientlessChannel)(nil)

func newTmuxClientlessChannel(ts *tmux.TmuxSession) *tmuxClientlessChannel {
	return &tmuxClientlessChannel{ts: ts}
}

// StartCapture makes a private FIFO, opens its read end, and enables pipe-pane to
// write the pane's raw output into it. The FIFO is opened O_RDWR so the reader
// stays valid even across the brief window before tmux's `cat` opens the write
// end (and never sees EOF from a transient writer close) — StopCapture is the
// only thing that ends the stream.
func (c *tmuxClientlessChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc != nil {
		return nil, fmt.Errorf("clientless capture already started")
	}

	dir, err := os.MkdirTemp("", "af-ptystream-")
	if err != nil {
		return nil, fmt.Errorf("create pty stream dir: %w", err)
	}
	fifo := filepath.Join(dir, "pane.out")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("mkfifo pty stream: %w", err)
	}
	f, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("open pty stream fifo: %w", err)
	}
	if err := c.ts.EnablePipePane(pipePaneCommand(fifo)); err != nil {
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	rc := &captureReader{f: f, fifo: fifo}
	c.dir, c.fifo, c.rc = dir, fifo, rc
	return rc, nil
}

// StopCapture disables pipe-pane, closes the read end, and removes the FIFO dir.
func (c *tmuxClientlessChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc == nil {
		return nil
	}
	err := c.ts.DisablePipePane()
	_ = c.rc.Close()
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
	c.dir, c.fifo, c.rc = "", "", nil
	return err
}

// SendRaw writes verbatim input bytes to the pane (clientless send-keys).
func (c *tmuxClientlessChannel) SendRaw(b []byte) error {
	return c.ts.SendRawKeys(b)
}

// Snapshot returns the pane's current visible screen as a GRID (one line per pane
// row, `capture-pane -p -e` WITHOUT -J) plus the pane cursor position — the repaint
// the broker injects so a fresh subscriber sees the actual screen. `pipe-pane` only
// streams FUTURE output, and — unlike a `tmux attach` client — never receives tmux's
// screen redraw, so without this a just-opened pane would render blank until the
// next byte of output (#1592 Phase 2 PR6). The grid form (not -J-joined) is what
// lets buildRepaint pin each row at its true position and land the restored cursor
// on the right line regardless of the client's width (#1688); the 0-based cursor_y
// then names the same row in the client that it named in the pane.
func (c *tmuxClientlessChannel) Snapshot() (PaneSnapshot, error) {
	content, err := c.ts.CaptureVisiblePaneGrid()
	if err != nil {
		return PaneSnapshot{}, err
	}
	snap := PaneSnapshot{Screen: []byte(content)}
	// Cursor and terminal modes are best-effort: a failure degrades to the old
	// screen-only repaint rather than failing the subscription. ReadTerminalState
	// gets both in one tmux request so a fresh client receives a coherent owner.
	if state, stateErr := c.ts.ReadTerminalState(); stateErr == nil {
		snap.CursorRow, snap.CursorCol, snap.HasCursor = state.CursorRow, state.CursorCol, true
		snap.Modes, snap.HasModes = state.Modes, true
	} else if row, col, curErr := c.ts.CursorPosition(); curErr == nil {
		// Preserve the cursor-only fallback for tmux versions/sources that cannot
		// expose the mode formats.
		snap.CursorRow, snap.CursorCol, snap.HasCursor = row, col, true
	}
	return snap, nil
}

// Resize applies the winning size to the window (clientless resize-window). tmux
// takes cols (x) then rows (y); the broker's rows/cols argument order is flipped
// to match here.
func (c *tmuxClientlessChannel) Resize(rows, cols uint16) error {
	return c.ts.ResizeWindow(int(cols), int(rows))
}

// pipePaneCommand builds the shell snippet handed to pipe-pane (tmux runs it
// through /bin/sh) to copy the pane's live output into the broker's FIFO. The
// FIFO path is single-quoted so a home dir with spaces or shell metacharacters
// cannot break the command.
//
// It uses `dd` rather than `cat` because `cat` is NOT reliably unbuffered across
// libc/coreutils implementations (#1592 Phase 4 PR4): busybox `cat` (musl/alpine,
// the common docker BYO image base) block-buffers its stdout when it is a pipe,
// so a session's live PTY output never streams — it sits in cat's buffer until
// the pane closes, which breaks the WS stream inside a container while working on
// a glibc host. `dd` does one read()→write() per block and writes each partial
// read immediately, so it streams promptly EVERYWHERE while a large block size
// keeps bulk output (a full-screen repaint, a fast log) as efficient as cat was.
// dd's completion stats go to stderr, discarded here.
func pipePaneCommand(fifoPath string) string {
	return "dd of=" + shellSingleQuote(fifoPath) + " bs=65536 2>/dev/null"
}

// shellSingleQuote single-quotes s for safe interpolation into a /bin/sh command,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
