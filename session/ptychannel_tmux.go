package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	dir  string   // temp dir holding the FIFO, removed on StopCapture
	fifo string   // FIFO path pipe-pane writes to
	rc   *os.File // read end of the FIFO
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
	rc, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("open pty stream fifo: %w", err)
	}
	if err := c.ts.EnablePipePane(pipePaneCommand(fifo)); err != nil {
		_ = rc.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
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

// Snapshot returns the pane's current visible screen with escape sequences
// (`capture-pane -p -e -J`) — the repaint the broker injects so a fresh subscriber
// (and every subscriber after a resize) sees the actual screen. `pipe-pane` only
// streams FUTURE output, and — unlike a `tmux attach` client — never receives
// tmux's screen redraw, so without this a just-opened or just-resized pane would
// render blank until the next byte of output (#1592 Phase 2 PR6).
func (c *tmuxClientlessChannel) Snapshot() ([]byte, error) {
	content, err := c.ts.CapturePaneContent()
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

// Resize applies the winning size to the window (clientless resize-window). tmux
// takes cols (x) then rows (y); the broker's rows/cols argument order is flipped
// to match here.
func (c *tmuxClientlessChannel) Resize(rows, cols uint16) error {
	return c.ts.ResizeWindow(int(cols), int(rows))
}

// pipePaneCommand builds the `cat >> <fifo>` shell snippet handed to pipe-pane
// (tmux runs it through /bin/sh). The FIFO path is single-quoted so a home dir
// with spaces or shell metacharacters cannot break the command.
func pipePaneCommand(fifoPath string) string {
	return "cat >> " + shellSingleQuote(fifoPath)
}

// shellSingleQuote single-quotes s for safe interpolation into a /bin/sh command,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
