// Package termpane renders a live tmux session inside a workspace pane
// (#1089, RFC §2.4 — architecture A, proven by the spike on branch
// spike/1089-embedded-terminal).
//
// A TermPane owns one attach client: a PTY running `tmux attach-session -t
// =<name>`, whose output is fed through a charmbracelet/x/vt terminal
// emulator into a cell grid. Render turns the grid into an ANSI-styled block
// the TUI places inside a pane rect — no full-screen takeover, the rail stays
// visible. Resize propagates the pane geometry to the PTY (tmux reflows on
// the SIGWINCH) and to the emulator grid in step. Close kills the attach
// CLIENT only; the tmux session keeps running server-side, exactly like a
// detach.
//
// This package is deliberately a pure projection: it never creates, kills, or
// mutates tmux sessions — the daemon stays the sole session owner (#960).
// tmux is also the flow limiter: the attach client receives visible-screen
// redraws, not the raw output firehose, so sustained streaming costs ~0.6% of
// one core (spike §2). Rendering is pulled by the TUI's existing tick/update
// cycle — this package runs no repaint loop of its own.
//
// PR 1 (#1089) is render-only: keystroke forwarding into the PTY is the next
// PR. The emulator's read side is already pumped back down the PTY, though,
// so replies to terminal queries (DA, DSR, ...) reach tmux and the attach
// handshake completes.
package termpane

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/sachiniyer/agent-factory/log"
)

// closeDrainDeadline bounds how long Close waits for the PTY reader goroutine
// to drain after the attach client is SIGKILLed. Killing the client closes
// the PTY slave end, which is what actually wakes a blocked master-side Read
// (closing the master alone does not — the #598 lesson in session/tmux), so
// the drain completes in milliseconds in practice. If it doesn't, Close
// returns anyway and leaks the goroutine: a leaked goroutine is strictly
// better than freezing the event loop, the same trade waitForAttachDrain
// makes.
const closeDrainDeadline = 2 * time.Second

// TermPane is one live embedded terminal: PTY + attach client + vt emulator.
//
// Concurrency: New, Resize, Render, and Close are called on the bubbletea
// event loop; the PTY reader and emulator-response pumps run on their own
// goroutines. gridMu is the synchronization point between them: the reader
// pump mutates the grid under the write lock, Render/Resize take it on the
// event loop. Deliberately NOT vt.SafeEmulator — its CellAt returns a
// pointer into the live buffer after releasing its per-call lock, so grid
// reads would still race the write pump (caught by -race); a pane-level
// RWMutex held across the whole frame is the correct scope anyway. Close is
// safe to call more than once.
type TermPane struct {
	cmd  *exec.Cmd
	ptmx *os.File

	gridMu sync.RWMutex
	emu    *vt.Emulator

	width, height int

	// done is closed when the PTY reader pump exits — on Close, or when the
	// attach client dies on its own (session killed, tmux server gone, or an
	// external detach). Owners poll Done to fall back to capture rendering.
	done      chan struct{}
	closeOnce sync.Once
}

// New opens a PTY running `tmux attach-session -t =<sessionName>` at the
// given grid size and starts pumping its output through the emulator. The
// `=` forces an exact session-name match, mirroring session/tmux's Restore
// (#1006) so a sibling session can never be prefix-matched instead.
func New(sessionName string, width, height int) (*TermPane, error) {
	return NewWithCommand(newAttachCommand(sessionName, os.Getenv("TMUX"), os.Environ()), width, height)
}

// newAttachCommand builds the attach client's argv and env. The TUI itself
// may be running inside tmux; strip $TMUX so the embedded client doesn't
// refuse to nest — but $TMUX is also where the server's socket path lives,
// so hand it back explicitly as `-S <path>`. Without that the child resolves
// TMUX_TMPDIR/default and, on a non-default socket (`tmux -L`/`-S`), attaches
// to the wrong server: it dies instantly while auto-starting a transient
// default-socket server as a side effect. With af outside tmux ($TMUX unset)
// the child's default resolution already matches every other af tmux call,
// so no -S is added. TERM is pinned to what the vt emulator implements so
// tmux emits escape sequences the grid understands.
func newAttachCommand(sessionName, tmuxEnv string, environ []string) *exec.Cmd {
	args := []string{"attach-session", "-t", "=" + sessionName}
	// $TMUX is `socket_path,server_pid,session_id`; the path is what -S wants.
	if sock, _, _ := strings.Cut(tmuxEnv, ","); sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	cmd := exec.Command("tmux", args...)
	env := []string{"TERM=xterm-256color"}
	for _, e := range environ {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") || strings.HasPrefix(e, "TERM=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	return cmd
}

// NewWithCommand is New with a caller-built command on the PTY instead of the
// default tmux attach argv. Tests use it to run scripted shells (no tmux
// server) and to point a real attach at a private `-L` socket.
func NewWithCommand(cmd *exec.Cmd, width, height int) (*TermPane, error) {
	width, height = clampSize(width, height)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)}) //nolint:gosec // clampSize bounds to [1, 4096]
	if err != nil {
		return nil, fmt.Errorf("termpane: starting %q on a PTY: %w", cmd.Path, err)
	}

	t := &TermPane{
		cmd:    cmd,
		ptmx:   ptmx,
		emu:    vt.NewEmulator(width, height),
		width:  width,
		height: height,
		done:   make(chan struct{}),
	}

	// Reap the client when it exits so it never lingers as a zombie. The
	// pump goroutines detect exit through the PTY, not through Wait.
	go func() { _ = cmd.Wait() }()

	// PTY → emulator. Exits when the client dies (slave end closes, Read
	// returns) or Close closes the master.
	go func() {
		defer close(t.done)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				t.gridMu.Lock()
				_, _ = t.emu.Write(buf[:n])
				t.gridMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Emulator → PTY: replies to terminal queries from tmux (DA, DSR, ...).
	// PR 2 sends encoded keystrokes down this same path. Exits when Close
	// closes the emulator's response pipe (Read then returns EOF).
	go func() { _, _ = io.Copy(ptmx, t.emu) }()

	return t, nil
}

// Resize propagates a new pane geometry: PTY winsize first (tmux reflows on
// the SIGWINCH), then the emulator grid in step. Event-loop only.
func (t *TermPane) Resize(width, height int) {
	width, height = clampSize(width, height)
	if width == t.width && height == t.height {
		return
	}
	t.width, t.height = width, height
	if err := pty.Setsize(t.ptmx, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)}); err != nil { //nolint:gosec // clampSize bounds to [1, 4096]
		log.WarningLog.Printf("termpane: pty resize to %dx%d failed: %v", width, height, err)
	}
	t.gridMu.Lock()
	t.emu.Resize(width, height)
	t.gridMu.Unlock()
}

// Render returns the emulator's current grid as exactly height ANSI-styled
// lines of exactly width cells. It only reads the grid — safe to call at any
// cadence; the TUI's existing tick drives it (no repaint loop here). After
// the attach client dies the last grid keeps rendering, so a pane shows its
// final frame until the owner notices Done and swaps render sources.
func (t *TermPane) Render(width, height int) string {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	return renderGrid(t.emu, width, height)
}

// Done reports attach-client death: the channel is closed once the PTY
// reader pump exits — after Close, or on its own when the tmux session is
// killed, the server dies, or the client is detached externally.
func (t *TermPane) Done() <-chan struct{} {
	return t.done
}

// Close tears down the attach CLIENT only — SIGKILL the tmux client child,
// close the PTY, close the emulator's response pipe — and waits (bounded) for
// the pumps to drain. The tmux session itself keeps running server-side;
// closing a pane must never kill the user's agent. SIGKILL rather than a
// graceful detach is deliberate: it is how session/tmux's hardened detach
// path ends every attach client too (#601/#602), and it guarantees the slave
// end closes so the reader pump wakes.
func (t *TermPane) Close() error {
	var err error
	t.closeOnce.Do(func() {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		// Wait for the reader pump BEFORE touching the emulator's pipes: the
		// pump may still be flushing the client's death throes through
		// emu.Write, and the emulator can block writing a query response
		// into its unbuffered response pipe mid-parse.
		select {
		case <-t.done:
		case <-time.After(closeDrainDeadline):
			err = fmt.Errorf("termpane: PTY reader did not drain within %v after kill; abandoning it", closeDrainDeadline)
		}
		_ = t.ptmx.Close()
		// Unblock the emulator→PTY pump. Deliberately NOT emu.Close(): the
		// pinned x/vt version sets an unsynchronized `closed` flag there
		// that races the pump's concurrent Read (caught by -race). The
		// response pipe's writer end is internally synchronized and closing
		// it never blocks; the pump's Read returns EOF and io.Copy exits.
		if pw, ok := t.emu.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
		} else {
			// Upstream changed the pipe plumbing: fall back to the racy-but-
			// functional Close rather than leaking the pump forever.
			_ = t.emu.Close()
		}
	})
	return err
}

// clampSize bounds a pane geometry to what a PTY winsize can hold and tmux
// will accept: at least 1x1 (a zero-rected auto-hidden pane must not produce
// a zero winsize), at most 4096 per axis (also keeps the uint16 casts safe).
func clampSize(width, height int) (int, int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	if width > 4096 {
		width = 4096
	}
	if height > 4096 {
		height = 4096
	}
	return width, height
}
