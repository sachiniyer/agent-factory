package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"golang.org/x/sys/unix"
)

// tmuxClientlessChannel is the local runtime's clientlessChannel (#1592 Phase 2
// PR5): it captures a tmux pane's output through `pipe-pane` into a private FIFO
// and drives input/size through `send-keys`/`resize-window` — all WITHOUT a
// `tmux attach-session` render client. The WS PTY broker fans the FIFO bytes to
// its subscribers; PR6 deletes the render client the TUI still uses today.
type tmuxClientlessChannel struct {
	ts *tmux.TmuxSession

	mu   sync.Mutex
	dir  string         // temp dir holding the FIFO, removed after its reader drains
	fifo string         // FIFO path pipe-pane writes to
	rc   *captureReader // read end of the FIFO
}

// captureReader wraps the FIFO's read end so capture teardown can drain every
// byte the pipe-pane writer committed before reporting EOF. The FIFO reader is
// opened read-only and a private keeper writer prevents transient writer exits
// from ending a live capture. Once tmux has disabled pipe-pane, closing that
// keeper lets the kernel's ordered FIFO EOF become the drain boundary: queued
// pane bytes (including NULs and chunks larger than the broker buffer) are read
// first, then EOF arrives after the real writer closes.
//
// A separate anonymous pipe is the failure-only escape hatch. Go cannot put a
// FIFO into the darwin netpoller, and close(2) does not interrupt an in-flight
// FIFO read, so Read uses poll(2) on both descriptors. If disabling pipe-pane
// fails and the external writer may remain open forever, Close wakes the poll
// out of band instead of placing an ambiguous sentinel in the raw PTY stream.
type captureReader struct {
	f         *os.File
	keepalive *os.File
	wakeR     *os.File
	wakeW     *os.File
	dir       string
	abort     atomic.Bool
	eof       atomic.Bool
	stopOnce  sync.Once
	cleanOnce sync.Once
	err       error
}

func (r *captureReader) Read(p []byte) (int, error) {
	if r.eof.Load() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	for {
		if r.abort.Load() {
			r.eof.Store(true)
			r.finish()
			return 0, io.EOF
		}
		pollFDs := []unix.PollFd{
			{Fd: int32(r.f.Fd()), Events: unix.POLLIN},
			{Fd: int32(r.wakeR.Fd()), Events: unix.POLLIN},
		}
		if _, err := unix.Poll(pollFDs, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			r.finish()
			return 0, fmt.Errorf("poll pty stream fifo: %w", err)
		}

		outputEvents := pollFDs[0].Revents
		if outputEvents&(unix.POLLIN|unix.POLLHUP) != 0 {
			n, err := r.f.Read(p)
			if n > 0 {
				return n, err
			}
			if errors.Is(err, syscall.EAGAIN) {
				continue
			}
			if errors.Is(err, io.EOF) || outputEvents&unix.POLLHUP != 0 {
				r.eof.Store(true)
				r.finish()
				return 0, io.EOF
			}
			if err != nil {
				r.finish()
				return 0, err
			}
		}
		if outputEvents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			r.finish()
			return 0, fmt.Errorf("poll pty stream fifo: events %#x", outputEvents)
		}

		wakeEvents := pollFDs[1].Revents
		if wakeEvents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 && r.abort.Load() {
			r.eof.Store(true)
			r.finish()
			return 0, io.EOF
		}
	}
}

func (r *captureReader) Close() error {
	return r.stop(false)
}

// stop closes the FIFO's private keeper. When tmux positively disabled its
// writer, drain=true lets Read consume through the kernel EOF. Otherwise the
// out-of-band wake prevents an unknown external writer from wedging teardown.
func (r *captureReader) stop(drain bool) error {
	r.stopOnce.Do(func() {
		if r.keepalive != nil {
			r.err = r.keepalive.Close()
		}
		if !drain {
			r.abort.Store(true)
			if r.wakeW != nil {
				_, _ = r.wakeW.Write([]byte{1})
			}
		}
	})
	return r.err
}

func (r *captureReader) finish() {
	r.cleanOnce.Do(func() {
		if r.f != nil {
			_ = r.f.Close()
		}
		if r.keepalive != nil {
			_ = r.keepalive.Close()
		}
		if r.wakeR != nil {
			_ = r.wakeR.Close()
		}
		if r.wakeW != nil {
			_ = r.wakeW.Close()
		}
		if r.dir != "" {
			_ = os.RemoveAll(r.dir)
		}
	})
}

var _ clientlessChannel = (*tmuxClientlessChannel)(nil)

func newTmuxClientlessChannel(ts *tmux.TmuxSession) *tmuxClientlessChannel {
	return &tmuxClientlessChannel{ts: ts}
}

// StartCapture makes a private FIFO, opens its read end, and enables pipe-pane to
// write the pane's raw output into it. A private writer keeps the FIFO live across
// the brief window before tmux opens its writer and across transient writer exits;
// StopCapture closes the keeper only after disabling pipe-pane, making FIFO EOF
// an ordered end-of-stream marker rather than injecting one into the PTY bytes.
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
	f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("open pty stream fifo: %w", err)
	}
	keepalive, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("keep pty stream fifo open: %w", err)
	}
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		_ = keepalive.Close()
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("create pty stream wake pipe: %w", err)
	}
	if err := c.ts.EnablePipePane(pipePaneCommand(fifo)); err != nil {
		_ = wakeW.Close()
		_ = wakeR.Close()
		_ = keepalive.Close()
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	rc := &captureReader{f: f, keepalive: keepalive, wakeR: wakeR, wakeW: wakeW, dir: dir}
	c.dir, c.fifo, c.rc = dir, fifo, rc
	return rc, nil
}

// StopCapture disables pipe-pane and starts an ordered drain. The capture reader
// closes its descriptors and removes the FIFO directory when it reaches EOF.
func (c *tmuxClientlessChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc == nil {
		return nil
	}
	err := c.ts.DisablePipePane()
	_ = c.rc.stop(err == nil)
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
