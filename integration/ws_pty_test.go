package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// TestWSPTYBrokerRoundTrip is the WS PTY broker integration harness (#1592 Phase
// 2 PR5), modelled on the remote-hook round-trip. Against a REAL daemon serving a
// REAL local tmux session (the fake-agent pane runs `cat`, so it echoes input) it
// exercises the whole data plane end to end:
//
//   - two subscribers both receive PTY_OUT bytes;
//   - INPUT is accepted from EITHER subscriber (multi-writer, no lease);
//   - RESIZE is last-resize-wins with an authoritative echo broadcast to all;
//   - dropping one subscriber mid-stream and reconnecting with ?since replays the
//     gap it missed (the ring-buffer repaint);
//   - a keepalive ping drops a dead subscriber WITHOUT killing the session — a
//     live subscriber keeps receiving and the session stays listed.
//
// Run it in the container fence: make ws-pty-roundtrip-container.
func TestWSPTYBrokerRoundTrip(t *testing.T) {
	// Shorten the broker's keepalive so the dead-subscriber drop is observable in
	// test time; the daemon (a child process) reads this from the environment.
	t.Setenv("AF_WS_KEEPALIVE_INTERVAL", "300ms")

	h := newHarness(t)
	h.startDaemon()
	h.createSession("wsstream")
	streamPath := "/v1/sessions/wsstream/stream"

	// --- two subscribers both receive PTY_OUT ---
	a := h.dialWS(t, streamPath)
	defer a.close()
	b := h.dialWS(t, streamPath)
	defer b.close()

	// --- INPUT from A → the pane echoes it; both subscribers see it ---
	a.sendInput(t, []byte("ping-from-a\n"))
	a.waitOutput(t, "ping-from-a")
	b.waitOutput(t, "ping-from-a")

	// --- INPUT from B → multi-writer: input from any subscriber is applied ---
	b.sendInput(t, []byte("ping-from-b\n"))
	a.waitOutput(t, "ping-from-b")
	b.waitOutput(t, "ping-from-b")

	// --- RESIZE: authoritative echo broadcast to every subscriber, last-wins ---
	// Resize from A, wait for BOTH to see the echo, THEN supersede from B — sending
	// both at once would race at the wire (two connections, no ordering), so this
	// deterministically pins both "echo reaches every subscriber" and "the last
	// resize wins" without a flaky ordering assumption.
	a.sendResize(t, 30, 100)
	a.waitResize(t, 30, 100)
	b.waitResize(t, 30, 100)
	b.sendResize(t, 40, 120) // multi-writer: B resizes too; last-resize-wins
	a.waitResize(t, 40, 120)
	b.waitResize(t, 40, 120)

	// --- drop A mid-stream + ?since replay repaints the gap ---
	since := a.sinceCursor()
	a.close()
	// B keeps the capture alive, so the ring retains the bytes A misses.
	b.sendInput(t, []byte("after-a-dropped\n"))
	b.waitOutput(t, "after-a-dropped")
	a2 := h.dialWS(t, fmt.Sprintf("%s?since=%d", streamPath, since))
	defer a2.close()
	a2.waitOutput(t, "after-a-dropped")

	// --- keepalive drops a dead subscriber without killing the session ---
	// A "dead" subscriber never reads after connecting, so it never pongs; the
	// server's ping times out and drops it. It must NOT take the session or the live
	// subscriber down.
	dead := h.dialWSRaw(t, streamPath)
	// A fresh subscriber receives two leading server→client frames before it goes
	// silent: the OpHello start-seq (#1592 Phase 5 PR1) then the one-shot initial
	// screen repaint (#1592 PR6). Drain BOTH first, otherwise the keepalive diagnostic
	// below would read one of those still-buffered frames (a non-error) instead of the
	// eventual close. After draining them the client stops reading, so the next ping
	// goes unanswered and the server drops it.
	for _, what := range []string{"hello", "repaint"} {
		if err := dead.readErrWithin(2 * time.Second); err != nil {
			t.Fatalf("dead subscriber never received its initial %s frame: %v", what, err)
		}
	}
	// Wait out several keepalive cycles (300ms ping + 300ms pong timeout).
	time.Sleep(1500 * time.Millisecond)
	// The server closed the dead subscriber: its next read now errors.
	if err := dead.readErrWithin(2 * time.Second); err == nil {
		t.Fatal("dead subscriber was not dropped by keepalive")
	}
	// The session survived: a live subscriber still receives fresh output...
	b.sendInput(t, []byte("after-dead\n"))
	b.waitOutput(t, "after-dead")
	// ...and it is still a live, listed session.
	if !hasTitle(h.listSessions(), "wsstream") {
		t.Fatal("session vanished after a dead subscriber was dropped")
	}
}

// TestWSPTYStreamTabRouting pins the tab-aware routing added in #1592 Phase 2 PR6:
// the ?tab= selector resolves a per-tab broker rather than always streaming the
// agent tab. An explicit ?tab=0 streams the agent pane (which echoes input), and a
// tab with no session is REJECTED at the handshake — never silently falling back
// to tab 0, which would stream the wrong pane's output into a shell-tab pane.
func TestWSPTYStreamTabRouting(t *testing.T) {
	h := newHarness(t)
	h.startDaemon()
	h.createSession("tabroute")

	// Explicit ?tab=0 streams the agent pane and accepts input (multi-writer).
	a := h.dialWS(t, "/v1/sessions/tabroute/stream?tab=0")
	defer a.close()
	a.sendInput(t, []byte("tab0-marker\n"))
	a.waitOutput(t, "tab0-marker")

	// A tab index with no tmux session must be refused at dial (the per-tab broker
	// can't resolve it), not accepted-then-silent. websocket.Dial returns an error
	// on the non-101 response the handler writes before the upgrade.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://unix/v1/sessions/tabroute/stream?tab=7",
		&websocket.DialOptions{HTTPClient: h.httpClient()})
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("dialing a nonexistent tab must be rejected, not silently routed to tab 0")
	}
}

// TestWSPTYSessionKillEmitsExit pins the #1592 Phase 5 PR5 terminal-exit fix: when
// a session is killed, its attached subscriber receives an explicit MsgExit control
// frame (the broker's session-ended signal) so a browser terminal settles to an
// "exited" state and stops reconnecting, rather than reconnect-looping against a
// gone session. Before the fix the broker only closed the socket, indistinguishable
// from a droppable network drop.
func TestWSPTYSessionKillEmitsExit(t *testing.T) {
	h := newHarness(t)
	h.startDaemon()
	h.createSession("wsexit")

	a := h.dialWS(t, "/v1/sessions/wsexit/stream")
	defer a.close()
	a.sendInput(t, []byte("alive-marker\n"))
	a.waitOutput(t, "alive-marker")

	// Kill the session; the broker closes and the writer sends MsgExit before the
	// socket closes.
	h.run("sessions", "kill", "wsexit")

	waitUntil(t, 5*time.Second, "subscriber received MsgExit on session kill", func() bool {
		return a.sawExit()
	})
}

// wsClient is a test subscriber: a coder/websocket connection with a read pump
// accumulating PTY_OUT bytes and resize echoes.
type wsClient struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	out     []byte
	resizes []agentproto.ResizeMessage
	exited  bool
	// cursor is the absolute replay cursor, tracked the way a real client tracks it
	// (ui/termpane, web/src/terminal.ts): seeded from the handshake seq, ADOPTED from
	// every HELLO frame, and advanced by each PTY_OUT byte. Deliberately NOT
	// handshake+len(out): the server re-seeds mid-stream when it moves the cursor over
	// bytes it discarded, and a client that ignored that would reconnect on a stale
	// ?since and be re-sent bytes it already rendered (the #1845 follow-up).
	cursor uint64
}

// dialWS opens a subscriber and starts its read pump.
func (h *harness) dialWS(t *testing.T, path string) *wsClient {
	t.Helper()
	conn, base := h.dialWSConn(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	c := &wsClient{conn: conn, cursor: base, ctx: ctx, cancel: cancel}
	go c.readPump()
	return c
}

// dialWSRaw opens a subscriber but starts NO read pump, so it never answers
// pings — the "dead" subscriber the keepalive must drop.
func (h *harness) dialWSRaw(t *testing.T, path string) *wsClient {
	t.Helper()
	conn, base := h.dialWSConn(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	return &wsClient{conn: conn, cursor: base, ctx: ctx, cancel: cancel}
}

func (h *harness) dialWSConn(t *testing.T, path string) (*websocket.Conn, uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://unix"+path, &websocket.DialOptions{HTTPClient: h.httpClient()})
	if err != nil {
		t.Fatalf("dial ws %s: %v", path, err)
	}
	conn.SetReadLimit(1 << 20)
	var base uint64
	if v := resp.Header.Get("X-Af-Stream-Seq"); v != "" {
		base, _ = strconv.ParseUint(v, 10, 64)
	}
	return conn, base
}

func (c *wsClient) readPump() {
	for {
		msg, err := agentproto.ReadMessage(c.ctx, c.conn)
		if err != nil {
			return
		}
		c.mu.Lock()
		if msg.Binary {
			switch msg.Frame.Op {
			case agentproto.OpPTYOut:
				c.out = append(c.out, msg.Frame.Data...)
				c.cursor += uint64(len(msg.Frame.Data))
			case agentproto.OpHello:
				// The server's authoritative cursor — the opening seed, or a mid-stream
				// re-seed announcing that it moved us over bytes it discarded. Adopt it
				// verbatim; our own byte count cannot see that jump.
				c.cursor = msg.Frame.Seq
			}
		} else if typ, _ := agentproto.MessageTypeOf(msg.Text); typ == agentproto.MsgResize {
			var rm agentproto.ResizeMessage
			if json.Unmarshal(msg.Text, &rm) == nil {
				c.resizes = append(c.resizes, rm)
			}
		} else if typ, _ := agentproto.MessageTypeOf(msg.Text); typ == agentproto.MsgExit {
			c.exited = true
		}
		c.mu.Unlock()
	}
}

func (c *wsClient) sendInput(t *testing.T, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agentproto.WriteFrame(ctx, c.conn, agentproto.InputFrame(b)); err != nil {
		t.Fatalf("send input: %v", err)
	}
}

func (c *wsClient) sendResize(t *testing.T, rows, cols uint16) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agentproto.WriteFrame(ctx, c.conn, agentproto.ResizeFrame(rows, cols)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
}

func (c *wsClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.out)
}

// sawExit reports whether this client received a MsgExit control frame — the
// broker's explicit session-ended signal (#1592 Phase 5 PR5).
func (c *wsClient) sawExit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exited
}

// sinceCursor is the absolute seq this client has consumed — what it would send as
// ?since on reconnect. See wsClient.cursor for why it is tracked rather than derived
// from len(out).
func (c *wsClient) sinceCursor() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor
}

func (c *wsClient) lastResize() (agentproto.ResizeMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.resizes) == 0 {
		return agentproto.ResizeMessage{}, false
	}
	return c.resizes[len(c.resizes)-1], true
}

func (c *wsClient) waitOutput(t *testing.T, want string) {
	t.Helper()
	waitUntil(t, 5*time.Second, fmt.Sprintf("output contains %q", want), func() bool {
		return strings.Contains(c.output(), want)
	})
}

func (c *wsClient) waitResize(t *testing.T, rows, cols uint16) {
	t.Helper()
	waitUntil(t, 5*time.Second, fmt.Sprintf("authoritative resize echo %dx%d", rows, cols), func() bool {
		r, ok := c.lastResize()
		return ok && r.Rows == rows && r.Cols == cols
	})
}

// readErrWithin performs one read with a deadline and returns its error; used to
// prove the server closed a dropped subscriber.
func (c *wsClient) readErrWithin(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, _, err := c.conn.Read(ctx)
	return err
}

func (c *wsClient) close() {
	c.cancel()
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}
