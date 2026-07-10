// Package termpane renders a live tmux session inside a workspace pane
// (#1089, RFC §2.4 — architecture A, proven by the spike on branch
// spike/1089-embedded-terminal).
//
// A TermPane owns one attach client: a PTY running `tmux attach-session -t
// =<name>`, whose output is fed through a charmbracelet/x/vt terminal
// emulator into a cell grid. Production tmux attachments are taller than the
// pane rect by the nested session's configured status height (0-5 rows) so
// tmux can draw its status line outside the rendered content window; Render
// turns only the visible grid area into an ANSI-styled block the TUI places
// inside a pane rect — no full-screen takeover, the rail stays visible. Resize
// propagates the pane geometry to the PTY (tmux reflows on the SIGWINCH) and
// to the emulator grid in step. Close kills the attach CLIENT only; the tmux
// session keeps running server-side, exactly like a detach.
//
// This package is deliberately a pure projection: it never creates, kills, or
// mutates tmux sessions — the daemon stays the sole session owner (#960).
// tmux is also the flow limiter: the attach client receives visible-screen
// redraws, not the raw output firehose, so sustained streaming costs ~0.6% of
// one core (spike §2). Rendering is pulled by the TUI's existing tick/update
// cycle — this package runs no repaint loop of its own.
//
// Interactive mode (#1089 PR 2) forwards focused keystrokes down the same
// PTY: SendKey translates each bubbletea key message (keymap.go) and hands it
// to the emulator's mode-aware encoder — or, for the modifier+navigation
// family the pinned x/vt cannot encode, writes the pre-encoded xterm sequence
// to the same input pipe — so keystrokes and terminal-query replies (DA, DSR,
// ...) reach tmux in order through one channel.
package termpane

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
const (
	closeDrainDeadline = 2 * time.Second

	// tmux clamps the status option to off/on or 2-5 rows. Embedded panes are
	// already framed by af, so production attachments allocate those rows but
	// crop them out of the rendered pane.
	maxTmuxStatusRows = 5
)

type statusPosition int

const (
	statusBottom statusPosition = iota
	statusTop
)

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
	// cursorVisible mirrors the terminal's DECTCM state, maintained by the
	// emulator's CursorVisibility callback — which fires inside emu.Write,
	// i.e. under gridMu's write lock; Render reads it under the read lock.
	// Starts true (terminals boot with the cursor shown).
	cursorVisible bool

	width, height  int
	statusRows     int
	statusPosition statusPosition

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
	tmuxEnv, environ := os.Getenv("TMUX"), os.Environ()
	rows, pos := tmuxStatusLayout(sessionName, tmuxEnv, environ)
	return newWithCommand(newAttachCommand(sessionName, tmuxEnv, environ), width, height, rows, pos)
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
	return newTmuxCommand(tmuxEnv, environ, args...)
}

func newTmuxCommand(tmuxEnv string, environ []string, args ...string) *exec.Cmd {
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

func tmuxStatusLayout(sessionName, tmuxEnv string, environ []string) (int, statusPosition) {
	status, err := tmuxShowSessionOption(sessionName, tmuxEnv, environ, "status")
	if err != nil {
		log.WarningLog.Printf("termpane: tmux status query for %s failed: %v (assuming one bottom status row)", sessionName, err)
		return 1, statusBottom
	}
	position, err := tmuxShowSessionOption(sessionName, tmuxEnv, environ, "status-position")
	if err != nil {
		log.WarningLog.Printf("termpane: tmux status-position query for %s failed: %v (assuming bottom)", sessionName, err)
		position = "bottom"
	}
	return parseTmuxStatusRows(status), parseTmuxStatusPosition(position)
}

func tmuxShowSessionOption(sessionName, tmuxEnv string, environ []string, option string) (string, error) {
	cmd := newTmuxCommand(tmuxEnv, environ, "show-options", "-Aqv", "-t", "="+sessionName+":", option)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", option, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func parseTmuxStatusRows(value string) int {
	switch strings.TrimSpace(value) {
	case "", "on":
		return 1
	case "off":
		return 0
	}
	rows, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 1
	}
	return clampStatusRows(rows)
}

func parseTmuxStatusPosition(value string) statusPosition {
	if strings.TrimSpace(value) == "top" {
		return statusTop
	}
	return statusBottom
}

// NewWithCommand is New with a caller-built command on the PTY instead of the
// default tmux attach argv. Tests use it to run scripted shells (no tmux
// server) and to point a real attach at a private `-L` socket.
func NewWithCommand(cmd *exec.Cmd, width, height int) (*TermPane, error) {
	return newWithCommand(cmd, width, height, 0, statusBottom)
}

func newWithCommand(cmd *exec.Cmd, width, height, statusRows int, pos statusPosition) (*TermPane, error) {
	width, height = clampSize(width, height)
	statusRows = clampStatusRows(statusRows)
	ptyHeight := ptyHeightForVisibleRows(height, statusRows)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(ptyHeight), Cols: uint16(width)}) //nolint:gosec // clampSize bounds to [1, 4096]
	if err != nil {
		return nil, fmt.Errorf("termpane: starting %q on a PTY: %w", cmd.Path, err)
	}

	t := &TermPane{
		cmd:            cmd,
		ptmx:           ptmx,
		emu:            vt.NewEmulator(width, ptyHeight),
		cursorVisible:  true,
		width:          width,
		height:         height,
		statusRows:     statusRows,
		statusPosition: pos,
		done:           make(chan struct{}),
	}
	// The callback fires from emu.Write — always under gridMu's write lock —
	// so the field write is ordered against Render's locked reads.
	t.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { t.cursorVisible = visible },
	})

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

	// Emulator → PTY: replies to terminal queries from tmux (DA, DSR, ...)
	// and the encoded keystrokes SendKey injects. This pump must keep
	// DRAINING the emulator's pipe even after the PTY write side dies (the
	// attach client exiting makes the next ptmx.Write fail): the pipe is
	// unbuffered and synchronous, so a pump that exited on write error would
	// leave the next SendKey — called on the bubbletea event loop — blocked
	// on it forever. Discard-once-dead instead; the pump exits when Close
	// closes the pipe (Read returns an error).
	go func() {
		buf := make([]byte, 4096)
		ptyAlive := true
		for {
			n, rerr := t.emu.Read(buf)
			if n > 0 && ptyAlive {
				if _, werr := ptmx.Write(buf[:n]); werr != nil {
					ptyAlive = false
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	return t, nil
}

// SendKey forwards one focused keystroke to the embedded terminal (#1089
// PR 2, interactive mode): pastes go through the emulator's bracketed-paste-
// aware Paste, everything else through the keymap.go translation. It reports
// whether the key was forwarded — false means the key has no safe encoding
// and was IGNORED (never a guessed byte sequence). Event-loop only; the
// enclosed pipe write is synchronous but the pump above always drains it.
//
// The lock is required even though this only WRITES input: the emulator's
// encoder reads terminal modes (DECCKM, bracketed paste) that the PTY reader
// pump mutates through emu.Write.
func (t *TermPane) SendKey(msg tea.KeyMsg) bool {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	return forwardKey(t.emu, msg)
}

// forwardKey is SendKey without the pane: translate one key message and emit
// its bytes into the emulator's input pipe. Factored out so the translation
// table can be pinned against a bare emulator in tests. The caller holds the
// lock that guards the emulator's mode state.
func forwardKey(emu *vt.Emulator, msg tea.KeyMsg) bool {
	if msg.Paste {
		emu.Paste(string(msg.Runes))
		return true
	}
	tr, ok := translateKey(msg)
	if !ok {
		return false
	}
	if tr.raw != "" {
		_, _ = io.WriteString(emu.InputPipe(), tr.raw)
		return true
	}
	for _, ev := range tr.events {
		emu.SendKey(ev)
	}
	return true
}

// Resize propagates a new pane geometry: PTY winsize first (tmux reflows on
// the SIGWINCH), then the emulator grid in step. Event-loop only.
func (t *TermPane) Resize(width, height int) {
	width, height = clampSize(width, height)
	if width == t.width && height == t.height {
		return
	}
	t.width, t.height = width, height
	ptyHeight := ptyHeightForVisibleRows(height, t.statusRows)
	if err := pty.Setsize(t.ptmx, &pty.Winsize{Rows: uint16(ptyHeight), Cols: uint16(width)}); err != nil { //nolint:gosec // clampSize bounds to [1, 4096]
		log.WarningLog.Printf("termpane: pty resize to %dx%d failed: %v", width, height, err)
	}
	t.gridMu.Lock()
	t.emu.Resize(width, ptyHeight)
	// The pinned x/vt emulator resizes by re-windowing the cell buffer, not by
	// reflowing it: a wrapped line is truncated to the new width and loses its
	// overflow, while the stale continuation row it wrapped onto stays behind
	// (#1556). Until tmux's SIGWINCH-driven full redraw lands, the grid can
	// therefore show a command's tail running straight into the next prompt —
	// prompts and commands visually merged. Blank the visible grid now so that
	// unavoidable transient reads as an empty pane filling in rather than a
	// corrupted transcript; tmux always full-redraws a resized client, so no
	// legitimate content is lost. The write goes through the same parser as
	// tmux's output and under the same lock, so it can never interleave with a
	// redraw mid-parse.
	_, _ = t.emu.Write(resizeBlank)
	t.gridMu.Unlock()
}

// resizeBlank erases the whole display (ED 2) and homes the cursor — the
// standard way a program clears a terminal. Written to the emulator after a
// resize to drop the truncated, un-reflowed grid the pinned x/vt leaves behind
// (#1556) before tmux repaints.
var resizeBlank = []byte("\x1b[2J\x1b[H")

// Render returns the emulator's current grid as exactly height ANSI-styled
// lines of exactly width cells. It only reads the grid — safe to call at any
// cadence; the TUI's existing tick drives it (no repaint loop here). After
// the attach client dies the last grid keeps rendering, so a pane shows its
// final frame until the owner notices Done and swaps render sources.
//
// showCursor overlays the terminal cursor (reverse-video on its cell) when
// the inner application has it visible — the interactive-mode typing cue
// (#1089 PR 2). Nav-mode panes render without it, exactly like PR 1.
func (t *TermPane) Render(width, height int, showCursor bool) string {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	cursor := cursorNone
	if showCursor && t.cursorVisible {
		pos := t.emu.CursorPosition()
		cursor = cursorAt{x: pos.X, y: pos.Y, show: true}
	}
	visibleHeight := min(height, t.height)
	sourceY := 0
	if t.statusPosition == statusTop {
		sourceY = t.statusRows
	}
	grid := renderGridWindow(t.emu, width, visibleHeight, sourceY, cursor)
	return padRenderedRows(grid, width, height-visibleHeight)
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

func clampStatusRows(rows int) int {
	if rows < 0 {
		return 0
	}
	if rows > maxTmuxStatusRows {
		return maxTmuxStatusRows
	}
	return rows
}

func ptyHeightForVisibleRows(height, statusRows int) int {
	ptyHeight := height + statusRows
	if ptyHeight > 4096 {
		return 4096
	}
	return ptyHeight
}
