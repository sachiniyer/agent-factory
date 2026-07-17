package apiclient

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// syncBuffer is a mutex-guarded buffer: the driver's reader goroutine writes it
// (stdout) while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// attachWSServer stands up a Unix-socket WS server that hands the accepted
// server-side conn to the test over connCh so the test can play the daemon's
// role (send PTY_OUT / MsgExit, read INPUT / MsgDetach). The handler blocks until
// cleanup so the socket stays open for the driver's lifetime.
func attachWSServer(t *testing.T) (*Client, <-chan *websocket.Conn) {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	connCh := make(chan *websocket.Conn, 1)
	hold := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Af-Stream-Seq", "0")
		conn, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		connCh <- conn
		<-hold // keep the handler (and conn) alive until the test finishes
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { close(hold); _ = srv.Close() })
	return NewWithSocket(sockPath), connCh
}

// startDriver dials the WS server and runs driveAttachStream against in-memory
// stdio (no TTY: oldState is nil, so the terminal restore is skipped). It returns
// the server-side conn, the pipe writer standing in for the user's keyboard, the
// captured stdout, and a channel closed when the driver exits.
// driverTermSize is the terminal-size seam startDriver installs as attachTermSize.
// Defaults to a non-TTY (no resize frames); a test sets it before startDriver to
// exercise the RESIZE writer. Read only on the main (sequential) test goroutine.
var driverTermSize = func() (uint16, uint16, bool) { return 0, 0, false }

func startDriver(t *testing.T) (srvConn *websocket.Conn, stdinW *io.PipeWriter, stdout *syncBuffer, done <-chan struct{}) {
	t.Helper()
	// Fast drain so a detach that the server doesn't promptly close still ends the
	// test quickly.
	prevDrain := attachDrainTimeout
	attachDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { attachDrainTimeout = prevDrain })

	c, connCh := attachWSServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sc, err := c.DialStream(ctx, "alpha", "", "", 0, 0)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	server := <-connCh

	inR, inW := io.Pipe()
	out := &syncBuffer{}
	prevIn, prevOut := attachStdin, attachStdout
	attachStdin, attachStdout = inR, out
	// driverTermSize defaults to a non-TTY (no resize frames); a test can set it
	// before startDriver to make the driver's initial sendResize actually fire.
	prevSize := attachTermSize
	attachTermSize = driverTermSize
	t.Cleanup(func() { attachStdin, attachStdout, attachTermSize = prevIn, prevOut, prevSize })

	d := make(chan struct{})
	go func() { defer close(d); driveAttachStream(sc.Conn, nil) }()
	return server, inW, out, d
}

func readServerMsg(t *testing.T, conn *websocket.Conn) agentproto.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	msg, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("server ReadMessage: %v", err)
	}
	return msg
}

// TestAttachStream_PTYOutToStdout: server PTY_OUT (and repaint) frames land on the
// attach client's stdout byte-for-byte.
func TestAttachStream_PTYOutToStdout(t *testing.T) {
	server, _, out, done := startDriver(t)
	ctx := context.Background()
	if err := agentproto.WriteFrame(ctx, server, agentproto.PTYOutFrame([]byte("hello "))); err != nil {
		t.Fatalf("write PTY_OUT: %v", err)
	}
	if err := agentproto.WriteFrame(ctx, server, agentproto.RepaintFrame([]byte("world"))); err != nil {
		t.Fatalf("write repaint: %v", err)
	}
	waitFor(t, func() bool { return out.String() == "hello world" }, "stdout should receive PTY_OUT + repaint bytes")

	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_DetachKeySendsDetachAndRestores: pressing the detach key sends
// a MsgDetach to the server, closes the attach, and writes the neutral terminal
// restore to stdout (the #845 local-edition restore).
func TestAttachStream_DetachKeySendsDetachAndRestores(t *testing.T) {
	server, stdinW, out, done := startDriver(t)

	if _, err := stdinW.Write([]byte{tmux.DetachKeyByte}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	msg := readServerMsg(t, server)
	if typ, _ := agentproto.MessageTypeOf(msg.Text); msg.Binary || typ != agentproto.MsgDetach {
		t.Fatalf("server expected a MsgDetach control frame, got %+v", msg)
	}
	// The server closes its side on detach (like the daemon's readPTYClient); the
	// driver's reader then ends and the attach tears down.
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
	if !strings.HasSuffix(out.String(), tmux.NeutralTerminalRestore) {
		t.Fatalf("stdout must end with the neutral terminal restore on detach; got %q", out.String())
	}
}

// TestAttachStream_BatchedDetachFlushesPrecedingInput is the #975 rule: when a
// single stdin read batches typed bytes with the detach key, the preceding bytes
// are forwarded as INPUT before the detach.
func TestAttachStream_BatchedDetachFlushesPrecedingInput(t *testing.T) {
	server, stdinW, _, done := startDriver(t)

	if _, err := stdinW.Write(append([]byte("abc"), tmux.DetachKeyByte)); err != nil {
		t.Fatalf("write batched detach: %v", err)
	}
	// First the preceding bytes as an INPUT frame...
	in := readServerMsg(t, server)
	if !in.Binary || in.Frame.Op != agentproto.OpInput || string(in.Frame.Data) != "abc" {
		t.Fatalf("expected INPUT 'abc' before detach, got %+v", in)
	}
	// ...then the detach.
	det := readServerMsg(t, server)
	if typ, _ := agentproto.MessageTypeOf(det.Text); det.Binary || typ != agentproto.MsgDetach {
		t.Fatalf("expected MsgDetach after the flushed input, got %+v", det)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_EncodedDetachKeyDetaches is the #1832 regression guard, at the
// level the bug actually bit: the whole attach driver, driven from stdin.
//
// A raw-proxied attach lets the pane program negotiate a richer keyboard encoding
// with the REAL terminal (claude emits `CSI > 1 u` and `CSI > 4 ; 2 m` at
// startup), after which the terminal reports ctrl+w as an escape sequence and
// NEVER as 0x17. Matching only the legacy byte forwarded the key to the agent —
// a harmless word-erase — so the user was trapped in the attach with no way back
// to the TUI. Each encoding here must produce a MsgDetach, not an INPUT frame.
func TestAttachStream_EncodedDetachKeyDetaches(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
	}{
		{"kitty CSI u (macOS + kitty + claude, #1832)", "\x1b[119;5u"},
		{"kitty CSI u with num-lock on", "\x1b[119;133u"},
		{"xterm modifyOtherKeys=2", "\x1b[27;5;119~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, stdinW, _, done := startDriver(t)

			if _, err := stdinW.Write([]byte(tc.seq)); err != nil {
				t.Fatalf("write encoded detach key: %v", err)
			}
			msg := readServerMsg(t, server)
			if typ, _ := agentproto.MessageTypeOf(msg.Text); msg.Binary || typ != agentproto.MsgDetach {
				t.Fatalf("encoded detach key %q must detach, not forward as INPUT; got %+v", tc.seq, msg)
			}
			_ = server.Close(websocket.StatusNormalClosure, "")
			waitClosed(t, done)
		})
	}
}

// TestAttachStream_EncodedDetachKeyFlushesPrecedingInput extends the #975 batching
// rule to the encoded forms: bytes typed just before the detach key still reach
// the agent, and only the key's own sequence is swallowed.
func TestAttachStream_EncodedDetachKeyFlushesPrecedingInput(t *testing.T) {
	server, stdinW, _, done := startDriver(t)

	if _, err := stdinW.Write([]byte("abc\x1b[119;5u")); err != nil {
		t.Fatalf("write batched encoded detach: %v", err)
	}
	in := readServerMsg(t, server)
	if !in.Binary || in.Frame.Op != agentproto.OpInput || string(in.Frame.Data) != "abc" {
		t.Fatalf("expected INPUT 'abc' before the encoded detach, got %+v", in)
	}
	det := readServerMsg(t, server)
	if typ, _ := agentproto.MessageTypeOf(det.Text); det.Binary || typ != agentproto.MsgDetach {
		t.Fatalf("expected MsgDetach after the flushed input, got %+v", det)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_BatchedKittyEventsDoNotLeakTheDetachKey guards the swallow
// contract when a pane program turns on kitty's report-event-types flag: one tap
// of ctrl+w is reported as a press AND a release, and a single stdin read can
// batch both.
//
// The detach key must not reach the agent on its way out. Suffix-matching alone
// sees only the trailing release, so the press half was forwarded as INPUT first
// — the swallowed key still mutating the pane (a word-erase in claude) at the
// moment the user leaves. The whole tap belongs to the detach.
func TestAttachStream_BatchedKittyEventsDoNotLeakTheDetachKey(t *testing.T) {
	server, stdinW, _, done := startDriver(t)

	// One tap, batched: press then release, as a terminal with event reporting on
	// delivers it.
	if _, err := stdinW.Write([]byte("\x1b[119;5:1u\x1b[119;5:3u")); err != nil {
		t.Fatalf("write batched press+release: %v", err)
	}
	// The very first frame must be the detach: anything before it is the detach
	// key leaking into the pane.
	msg := readServerMsg(t, server)
	if msg.Binary && msg.Frame.Op == agentproto.OpInput {
		t.Fatalf("the press half of the detach tap leaked to the agent as INPUT %q; "+
			"one tap is one keypress and must be swallowed whole", msg.Frame.Data)
	}
	if typ, _ := agentproto.MessageTypeOf(msg.Text); msg.Binary || typ != agentproto.MsgDetach {
		t.Fatalf("expected MsgDetach for the batched tap, got %+v", msg)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_EncodedNonDetachKeyForwards is the other half of the contract:
// widening detection must not start swallowing keys the agent needs. ctrl+shift+w
// under the same kitty encoding differs from the detach key only in its modifier
// bits, so it is the sharpest available tripwire for an over-broad match.
func TestAttachStream_EncodedNonDetachKeyForwards(t *testing.T) {
	server, stdinW, _, done := startDriver(t)

	const ctrlShiftW = "\x1b[119;6u"
	if _, err := stdinW.Write([]byte(ctrlShiftW)); err != nil {
		t.Fatalf("write ctrl+shift+w: %v", err)
	}
	in := readServerMsg(t, server)
	if !in.Binary || in.Frame.Op != agentproto.OpInput || string(in.Frame.Data) != ctrlShiftW {
		t.Fatalf("ctrl+shift+w must forward to the agent verbatim, got %+v", in)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_InputForwarded: plain keystrokes reach the server as INPUT.
func TestAttachStream_InputForwarded(t *testing.T) {
	server, stdinW, _, done := startDriver(t)

	if _, err := stdinW.Write([]byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	in := readServerMsg(t, server)
	if !in.Binary || in.Frame.Op != agentproto.OpInput || string(in.Frame.Data) != "x" {
		t.Fatalf("expected INPUT 'x', got %+v", in)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_ConcurrentWritersSerialized exercises the multi-writer path:
// the initial RESIZE (sent from the driver's main goroutine when a terminal size
// is available) can race a stdin INPUT frame (sent from the stdin goroutine) on
// the one WS conn — which coder/websocket forbids. The driver funnels both
// through a single write mutex; run under -race this proves they don't collide,
// and the server must receive BOTH frames intact (order-independent).
func TestAttachStream_ConcurrentWritersSerialized(t *testing.T) {
	// Force a real terminal size so the driver's initial sendResize actually
	// writes a RESIZE frame (the default seam returns ok=false). Set BEFORE
	// startDriver so driveAttachStream captures it.
	prevSize := driverTermSize
	driverTermSize = func() (uint16, uint16, bool) { return 24, 80, true }
	t.Cleanup(func() { driverTermSize = prevSize })

	server, stdinW, _, done := startDriver(t)
	// Type immediately so the INPUT write overlaps the initial RESIZE write.
	if _, err := stdinW.Write([]byte("hi")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	sawResize, sawInput := false, false
	for i := 0; i < 2; i++ {
		msg := readServerMsg(t, server)
		switch {
		case msg.Binary && msg.Frame.Op == agentproto.OpResize:
			sawResize = true
		case msg.Binary && msg.Frame.Op == agentproto.OpInput && string(msg.Frame.Data) == "hi":
			sawInput = true
		}
	}
	if !sawResize || !sawInput {
		t.Fatalf("expected both RESIZE and INPUT('hi') intact from serialized writers; resize=%v input=%v", sawResize, sawInput)
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}

// TestAttachStream_ServerExitClosesAttach: a server-side MsgExit (pane ended)
// tears the attach down without the client pressing the detach key.
func TestAttachStream_ServerExitClosesAttach(t *testing.T) {
	server, _, _, done := startDriver(t)
	// Drain the server conn so the client's post-exit close handshake completes
	// promptly — the real daemon reads/acks the close, but this test's handler is
	// parked, so without a reader the client's conn.Close would block on the
	// closing handshake. (Concurrent read + single write is allowed by the lib.)
	go func() {
		for {
			if _, _, err := server.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	if err := agentproto.WriteControl(context.Background(), server, agentproto.NewExitMessage(0)); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	waitClosed(t, done)
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", msg)
}

func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("attach driver did not exit within timeout")
	}
}
