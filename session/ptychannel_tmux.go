package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
// its subscribers, and the instance/tab drivers (TUI, web, CLI) stream through
// it — their attach-session render client is gone (retired in #1592 Phase 2
// PR7). The config-agent terminal takeover is the remaining exception: it hands
// the terminal to a real `tmux attach-session` (app/config_agent.go, #2019).
type tmuxClientlessChannel struct {
	ts *tmux.TmuxSession

	mu sync.Mutex
	rc *captureReader // read end of the FIFO
}

// captureReader wraps the FIFO's read end so capture teardown can drain every
// byte the pipe-pane writer committed before reporting EOF. The FIFO reader is
// opened read-only and a private keeper writer prevents transient writer exits
// from ending a live capture. Once tmux has disabled pipe-pane, closing that
// keeper lets the kernel's ordered FIFO EOF become the drain boundary: queued
// pane bytes (including NULs and chunks larger than the broker buffer) are read
// first, then EOF arrives after the real writer closes.
//
// If disabling pipe-pane fails, its writer may remain open forever. That
// already-aborted path writes one wake byte through the keeper before closing
// it; Read sees the abort latch and discards the whole read, so the byte can
// never be confused with successful raw PTY output. Successful teardown never
// injects a control byte.
//
// Drain and abort are SEPARATE latches (#7302). Close is always an abort, even
// after a draining stop: it latches abort and writes its wake byte, through the
// keeper while it is open and otherwise through a fresh writer on the FIFO,
// because the drain has already closed the keeper. When one latch served both,
// the drain spent it, the abort that followed was a no-op, and a read whose EOF
// never came stayed parked for good: a writer that outlived pipe-pane, or the
// macOS read that stayed in read(2) after the last writer closed (#4319, #7302).
// The broker gives the drain a grace to reach EOF before it aborts
// (ptyBroker.joinCapture), so a healthy teardown still loses no tail.
//
// The wake writes only into the FIFO this reader opened, and only while the
// reader still owns it (see wake). It never blocks: a byte the FIFO cannot take
// at once is dropped.
type captureReader struct {
	f         *os.File
	keepalive *os.File
	// fifo is the FIFO's path: the route an abort's wake takes once a drain has
	// closed the keeper. Empty for a reader with no path, which then has no wake
	// left after a drain. fifoDev and fifoIno identify the FIFO the read end
	// opened; the wake writes only to a descriptor that is that same FIFO.
	fifo      string
	fifoDev   uint64
	fifoIno   uint64
	dir       string
	abort     atomic.Bool
	eof       atomic.Bool
	drainOnce sync.Once
	abortOnce sync.Once
	cleanOnce sync.Once
	drainErr  error
	abortErr  error
	// keeperMu orders the keeper's single close, and finish's release of the
	// FIFO, against a wake: a wake never writes to a closed (and possibly reused)
	// descriptor, and never reaches for the path once finished is latched.
	keeperMu     sync.Mutex
	keeperClosed bool
	keeperErr    error
	finished     bool
}

func (r *captureReader) Read(p []byte) (int, error) {
	if r.eof.Load() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	if r.abort.Load() {
		r.eof.Store(true)
		r.finish()
		return 0, io.EOF
	}
	n, err := r.f.Read(p)
	if r.abort.Load() {
		r.eof.Store(true)
		r.finish()
		return 0, io.EOF
	}
	if err != nil {
		if err == io.EOF {
			r.eof.Store(true)
		}
		r.finish()
	}
	return n, err
}

func (r *captureReader) Close() error {
	return r.stop(false)
}

// stop closes the FIFO's private keeper. When tmux positively disabled its
// writer, drain=true lets Read consume through the kernel EOF. Otherwise the
// keeper's failure-only byte prevents an unknown external writer from wedging
// teardown; Read discards it because abort is already latched.
//
// Each mode has its own latch, so a drain never consumes the abort: an abort
// that follows a drain still runs its wake (#7302).
func (r *captureReader) stop(drain bool) error {
	if drain {
		r.drainOnce.Do(func() { r.drainErr = r.closeKeeper() })
		return r.drainErr
	}
	r.abortOnce.Do(func() {
		r.abort.Store(true)
		r.wake()
		r.abortErr = r.closeKeeper()
	})
	return r.abortErr
}

// wake puts one byte where a parked Read will see it; abort is latched first, so
// Read discards it. It goes through the keeper while the keeper is open, and
// after a drain has closed the keeper, through a fresh writer on the FIFO.
//
// The wake writes only into the FIFO this reader opened, and only while the
// reader still owns it:
//
//   - Never once finish has begun. finish latches finished under keeperMu before
//     it closes the read end or removes the directory, and this checks it under
//     the same lock. Until then the directory is this process's own, mode 0700.
//     After it, the path names nothing this reader owns, and an ordinary
//     teardown closes every reader after its loop has already finished.
//   - Never through a link, and never into anything but that FIFO. The path is
//     opened with O_NOFOLLOW (and O_NOCTTY, so even a device at the path cannot
//     become a controlling terminal), and the byte is written only if fstat names
//     a FIFO with the read end's device and inode.
//
// It never blocks. Each route makes one nonblocking write attempt and drops a
// byte the FIFO cannot take at once. That is safe: a full FIFO already holds
// data for the read to return, and abort is already latched.
func (r *captureReader) wake() {
	r.keeperMu.Lock()
	defer r.keeperMu.Unlock()
	if r.finished {
		return
	}
	if !r.keeperClosed && r.keepalive != nil {
		writeWakeByteOnce(r.keepalive)
		return
	}
	if r.fifo == "" {
		return
	}
	fd, err := syscall.Open(r.fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return // ENXIO: the read end is gone; ELOOP: a link; either way, nothing of ours to wake
	}
	defer func() { _ = syscall.Close(fd) }()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFIFO || uint64(st.Dev) != r.fifoDev || uint64(st.Ino) != r.fifoIno {
		return
	}
	_, _ = syscall.Write(fd, wakeByte)
}

// wakeByte is the abort's single wake byte. Read discards it, because abort is
// latched before it is written.
var wakeByte = []byte{0}

// writeWakeByteOnce makes ONE write attempt through f and never parks on the
// netpoller, which a plain f.Write would do on a full FIFO. The keeper is opened
// O_NONBLOCK, so a FIFO that cannot take the byte refuses it with EAGAIN, and the
// byte is dropped.
func writeWakeByteOnce(f *os.File) {
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Write(func(fd uintptr) bool {
		_, _ = syscall.Write(int(fd), wakeByte)
		return true
	})
}

func (r *captureReader) closeKeeper() error {
	r.keeperMu.Lock()
	defer r.keeperMu.Unlock()
	if !r.keeperClosed {
		r.keeperClosed = true
		if r.keepalive != nil {
			r.keeperErr = r.keepalive.Close()
		}
	}
	return r.keeperErr
}

func (r *captureReader) finish() {
	r.cleanOnce.Do(func() {
		// Latch BEFORE releasing anything: from here on this reader no longer
		// owns its FIFO's path, so a wake must not reach for it (see wake).
		r.keeperMu.Lock()
		r.finished = true
		r.keeperMu.Unlock()
		if r.f != nil {
			_ = r.f.Close()
		}
		_ = r.closeKeeper()
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
	// Open the read side nonblocking only long enough to establish the keeper;
	// wrapping the descriptor after restoring blocking mode deliberately keeps
	// FIFOs out of Go's kqueue/netpoll path on Darwin (where writer close does not
	// produce a readiness event).
	fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("open pty stream fifo: %w", err)
	}
	keepalive, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		_ = syscall.Close(fd)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("keep pty stream fifo open: %w", err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = keepalive.Close()
		_ = syscall.Close(fd)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("make pty stream fifo blocking: %w", err)
	}
	// The read end's identity, so a later wake can prove a descriptor it opens
	// by path is this same FIFO.
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = keepalive.Close()
		_ = syscall.Close(fd)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("stat pty stream fifo: %w", err)
	}
	f := os.NewFile(uintptr(fd), fifo)
	if err := c.ts.EnablePipePane(pipePaneCommand(fifo)); err != nil {
		_ = keepalive.Close()
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	rc := &captureReader{f: f, keepalive: keepalive, fifo: fifo, fifoDev: uint64(st.Dev), fifoIno: uint64(st.Ino), dir: dir}
	c.rc = rc
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
	c.rc = nil
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
	return tmuxPaneSnapshot(c.ts)
}

// tmuxPaneSnapshot is the capture behind tmuxClientlessChannel.Snapshot, a
// function of the pane alone so the agent-server's grid preview serves a remote
// subscriber from the very same code (#5571 W4.2) rather than a second capture.
func tmuxPaneSnapshot(ts *tmux.TmuxSession) (PaneSnapshot, error) {
	// A held dead pane has no future output for the repaint to precede, but a
	// subscriber still needs its screen (#5330): pipe-pane already refused it,
	// and the visible grid alone is not the answer — tmux pushed content into
	// history when it drew the "Pane is dead" marker, and on the lost-SIGCHLD
	// shapes it never draws (#4682) the grid is simply the pre-death screen.
	// Capture the full buffer and bottom-anchor it below.
	dead := ts.PaneDead()
	var content string
	var err error
	if dead {
		content, err = ts.CapturePaneBuffer()
	} else {
		content, err = ts.CaptureVisiblePaneGrid()
	}
	if err != nil {
		return PaneSnapshot{}, err
	}
	snap := PaneSnapshot{Screen: []byte(content)}
	// Cursor and terminal modes are best-effort: a failure degrades to the old
	// screen-only repaint rather than failing the subscription. ReadTerminalState
	// gets both in one tmux request so a fresh client receives a coherent owner.
	if state, stateErr := ts.ReadTerminalState(); stateErr == nil {
		snap.CursorRow, snap.CursorCol, snap.HasCursor = state.CursorRow, state.CursorCol, true
		snap.Modes, snap.HasModes = state.Modes, true
		snap.CursorVisible, snap.HasVisibility = state.CursorVisible, true
		// The pane's measured dimensions — the only size a never-driven pane ever
		// reports (#4480). Guard the uint16 narrowing so a pathological format
		// answer degrades to "unmeasured" rather than wrapping.
		if state.PaneWidth > 0 && state.PaneWidth <= 0xFFFF && state.PaneHeight > 0 && state.PaneHeight <= 0xFFFF {
			snap.Cols, snap.Rows, snap.HasSize = uint16(state.PaneWidth), uint16(state.PaneHeight), true
		}
	} else if row, col, curErr := ts.CursorPosition(); curErr == nil {
		// Preserve the cursor-only fallback for tmux versions/sources that cannot
		// expose the mode formats. Visibility is unknown in this path — the
		// repaint leaves the emulator's default cursor visibility untouched.
		snap.CursorRow, snap.CursorCol, snap.HasCursor = row, col, true
	}
	if !dead {
		return snap, nil
	}
	if !snap.HasSize {
		// The buffer's tail can only be cut safely to a KNOWN height —
		// buildRepaint places rows absolutely, so lines past the emulator's
		// height collapse onto its last row. With no measured height the
		// visible grid is the safe stand-in: it is pane-height by construction
		// and already carries tmux's marker when tmux drew one.
		content, err = ts.CaptureVisiblePaneGrid()
		if err != nil {
			return PaneSnapshot{}, err
		}
	}
	snap.Screen = deadPaneScreen(content, int(snap.Rows))
	// A dead pane has no live cursor to restore; leave the emulator's where the
	// repaint ends, on the marker row. A viewer's crop anchors on that row
	// (#4480), and the output sits directly above it, so any box shows both
	// (#7061).
	snap.CursorRow, snap.CursorCol, snap.HasCursor = 0, 0, false
	return snap, nil
}

// PaneDead reports the pane's pane_dead flag: its terminal is gone, so
// pipe-pane refuses it and the stream is terminal rather than recoverable
// (#5330). See TmuxSession.PaneDead.
func (c *tmuxClientlessChannel) PaneDead() bool {
	return c.ts.PaneDead()
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
// Linux uses `dd` rather than `cat` because `cat` is NOT reliably unbuffered across
// libc/coreutils implementations (#1592 Phase 4 PR4): busybox `cat` (musl/alpine,
// the common docker BYO image base) can hold a live session's bytes until the pane
// closes. Darwin is the inverse: BSD dd waits for its full bs=65536 input block, so
// ordinary prompt-sized writes never reach the FIFO. Its system cat copies those
// partial reads promptly, as the real macOS pipe-pane gate proves (#1945).
func pipePaneCommand(fifoPath string) string {
	return pipePaneCommandForGOOS(fifoPath, runtime.GOOS)
}

func pipePaneCommandForGOOS(fifoPath, goos string) string {
	if goos == "darwin" {
		return "cat > " + shellSingleQuote(fifoPath)
	}
	return "dd of=" + shellSingleQuote(fifoPath) + " bs=65536 2>/dev/null"
}

// shellSingleQuote single-quotes s for safe interpolation into a /bin/sh command,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
