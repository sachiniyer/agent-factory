package apiclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Seams for the terminal the driver proxies to/from. Production wires them to the
// real terminal; tests swap them for pipes/buffers to exercise the WS↔stdio
// proxy without a TTY. attachTermSize returns the current terminal size and false
// when it can't be read (e.g. stdin is not a TTY — no resize frame is sent).
var (
	attachStdin    io.Reader = os.Stdin
	attachStdout   io.Writer = os.Stdout
	attachTermSize           = func() (rows, cols uint16, ok bool) {
		c, r, err := term.GetSize(int(os.Stdin.Fd()))
		if err != nil {
			return 0, 0, false
		}
		return uint16(r), uint16(c), true
	}
)

// Client-side full-screen attach over the WS PTY stream (#1592 Phase 2 PR7).
// This is the interactive sibling of app/live_stream.go's embedded-pane consumer:
// where a live pane feeds the ui/termpane emulator, a full-screen attach proxies
// the raw PTY byte stream straight to the REAL terminal — exactly what the retired
// `tmux attach-session` driver and the remote hook attach do, but sourced from the
// daemon's clientless broker over the same daemon-http.sock the read/control
// client uses. It serves both the TUI (app) and `af sessions attach` (api),
// which is why it lives in apiclient rather than app.
//
// MULTI-WRITER, no lease: the attaching client is just another subscriber. It
// sends INPUT/RESIZE and receives PTY_OUT; RESIZE is last-resize-wins with the
// broker's authoritative echo, so the #598 size-fight is structurally absent —
// there is no second tmux client forcing the window size. Detach is a clean
// WS-level signal (MsgDetach) plus a bounded drain; the pane is never killed.

// attachDrainTimeout bounds how long the detach path waits for the server to
// flush its remaining PTY_OUT and close its side after we send MsgDetach, before
// force-closing the socket to unblock the reader. Mirrors the hook attach's
// hookAttachDrainTimeout (#912): generous for a genuine drain, bounded against a
// server that never closes.
var attachDrainTimeout = 2 * time.Second

// AttachStream opens a full-screen interactive attach to tab `tab` of the session
// (title, optional repoID) over the WS PTY stream and returns a channel closed
// when the user detaches (Ctrl-<detach-key>) or the pane exits. It puts the real
// terminal into raw mode, proxies stdin→INPUT / PTY_OUT→stdout / SIGWINCH→RESIZE,
// and restores the terminal on exit. The signature matches the attach seam
// (app.attachOverlayCallback's `func() (chan struct{}, error)`), so local
// full-screen attach drops in where the tmux driver used to be.
func (c *Client) AttachStream(ctx context.Context, title, repoID, tabID string, tab int) (chan struct{}, error) {
	sc, err := c.DialStream(ctx, title, repoID, tabID, tab, 0) // 0 = live tail
	if err != nil {
		return nil, err
	}
	in, err := newAttachInputReader(attachStdin)
	if err != nil {
		_ = sc.Conn.Close(websocket.StatusInternalError, "stdin cancellation")
		return nil, fmt.Errorf("prepare cancellable stdin: %w", err)
	}
	// The tmux/ssh client used to own local terminal setup; the clientless proxy
	// must do it itself so arrow keys, Ctrl sequences and the detach key arrive
	// byte-for-byte and nothing echoes locally. For the CLI (no bubbletea) this is
	// the only thing putting the terminal into raw mode; for the TUI it snapshots
	// bubbletea's termios so Restore hands it back exactly.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		_ = in.Close()
		_ = sc.Conn.Close(websocket.StatusInternalError, "raw mode")
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		driveAttachStream(sc.Conn, oldState, in)
	}()
	return done, nil
}

// newAttachInputReader gives the attach exclusive, cancellable ownership of
// stdin. Tests may provide their own CancelReader to model terminal FIFO order;
// production wraps os.Stdin with the same cancelreader Bubble Tea uses.
func newAttachInputReader(in io.Reader) (cancelreader.CancelReader, error) {
	if cancellable, ok := in.(cancelreader.CancelReader); ok {
		return cancellable, nil
	}
	return cancelreader.NewReader(in)
}

// attachWriteTimeout bounds a single WS write so a wedged server can't pin the
// write mutex (and thereby every writer goroutine) forever.
var attachWriteTimeout = 10 * time.Second

// driveAttachStream runs the loops of a full-screen attach — WS→stdout, stdin→WS
// (with detach-key detection), and SIGWINCH→RESIZE — until the stream ends, then
// restores the terminal. It owns the terminal for its lifetime. The caller
// supplies a cancellable stdin reader so a server-side exit can unblock the
// input pump before Bubble Tea starts its replacement reader (#2671).
//
// Concurrency: THREE goroutines can write the WS conn (INPUT from stdin, RESIZE
// from SIGWINCH, the detach control frame), but coder/websocket permits only ONE
// concurrent writer, so every write funnels through writeMu — a single serialized
// writer, not one-lock-per-anything. The reader runs independently (one reader +
// one writer is allowed). conn.Close is idempotent via closeOnce (both the drain
// timer and the drain-complete path close it). The io seams are captured as
// locals up front so no goroutine re-reads package-level vars a test swaps back
// in Cleanup.
func driveAttachStream(conn *websocket.Conn, oldState *term.State, in cancelreader.CancelReader) {
	defer in.Close()
	// Snapshot EVERY package-level seam/tunable once, here, before any goroutine
	// spawns. The goroutines then read only these locals — never the package vars
	// a test swaps and restores in Cleanup. This single entry read is sequenced
	// before the workers and before `done`, so it is ordered against the test's
	// restore.
	out, termSize := attachStdout, attachTermSize
	drainTimeout, writeTimeout := attachDrainTimeout, attachWriteTimeout

	// The read context is deliberately NOT cancelled on detach: after MsgDetach we
	// keep reading so the server's final PTY_OUT drains to the terminal before the
	// socket closes (the #912 drain, WS edition). The bounded closeConn timer is
	// what guarantees termination.
	ctx := context.Background()

	// The single serialized WS writer. All three writer goroutines go through it.
	var writeMu sync.Mutex
	writeConn := func(write func(context.Context) error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return write(wctx)
	}
	writeFrame := func(f agentproto.Frame) error {
		return writeConn(func(c context.Context) error { return agentproto.WriteFrame(c, conn, f) })
	}

	var closeOnce sync.Once
	closeConn := func() {
		closeOnce.Do(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	}

	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			// Clean-close signal to the server (pane survives — multi-writer, no
			// lease). readPTYClient returns on MsgDetach and closes its side, which
			// ends our reader; the timer is the backstop if it never does.
			_ = writeConn(func(c context.Context) error {
				return agentproto.WriteControl(c, conn, agentproto.NewDetachMessage())
			})
			time.AfterFunc(drainTimeout, closeConn)
		})
	}

	// WS → stdout, byte-for-byte. Sole writer of `out`, ordered before the final
	// restore write by copyDone.
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		for {
			msg, err := agentproto.ReadMessage(ctx, conn)
			if err != nil {
				return // detach force-close, server close, or stream error
			}
			if msg.Binary {
				switch msg.Frame.Op {
				case agentproto.OpPTYOut, agentproto.OpRepaint:
					_, _ = out.Write(msg.Frame.Data)
				}
				continue
			}
			// A peer's MsgResize can't reflow a full-screen REAL terminal to a
			// foreign size (our own SIGWINCH re-asserts ours), so it is ignored —
			// the structural absence of the #598 size-fight. MsgExit ends the attach.
			if t, _ := agentproto.MessageTypeOf(msg.Text); t == agentproto.MsgExit {
				return
			}
		}
	}()

	// stdin → INPUT, with detach-key detection. stdin.Read can batch the detach
	// key with preceding bytes in a single read (buffered terminal input), so
	// check the END of the chunk rather than requiring the key to be the sole
	// bytes, forwarding anything before it first (#975).
	//
	// The detach key is matched in every encoding the pane program may have
	// negotiated onto the real terminal, not just the legacy C0 byte: an agent
	// CLI that turns on the kitty keyboard protocol or modifyOtherKeys makes the
	// terminal report ctrl+<key> as an escape sequence, and matching 0x17 alone
	// left the user unable to detach at all (#1832). See detachkey.go.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 32)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				if keyLen := trailingDetachKeyLen(buf[:n]); keyLen > 0 {
					if n > keyLen {
						_ = writeFrame(agentproto.InputFrame(buf[:n-keyLen]))
					}
					detach()
					return
				}
				if werr := writeFrame(agentproto.InputFrame(buf[:n])); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// SIGWINCH → RESIZE, plus an initial RESIZE on connect so the pane matches the
	// terminal immediately (the job the retired monitorWindowSize's forced initial
	// SIGWINCH used to do).
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	sendResize := func() {
		rows, cols, ok := termSize()
		if !ok {
			return
		}
		_ = writeFrame(agentproto.ResizeFrame(rows, cols))
	}
	sendResize()
	go func() {
		for {
			select {
			case <-copyDone:
				return
			case <-winch:
				sendResize()
			}
		}
	}()

	<-copyDone
	// A server close/MsgExit can end copyDone while stdin is still blocked. Cancel
	// that read and wait for the pump to prove it has exited before restoring the
	// terminal; requesting cancellation alone is not proof the reader is gone.
	_ = in.Cancel()
	closeConn()
	<-stdinDone
	// The stream is fully drained (copyDone), so this lands strictly after any
	// modes the pane program set — neutralize the terminal (#845, local edition),
	// then hand the termios back to whatever owned it before attach (bubbletea for
	// the TUI, the shell for the CLI). oldState is nil only in tests that drive the
	// proxy without a real TTY.
	_, _ = io.WriteString(out, tmux.NeutralTerminalRestore)
	if oldState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}
}
