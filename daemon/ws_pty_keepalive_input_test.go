package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// #7105: the keepalive drops a subscriber only when its peer stopped answering.
// coder/websocket v1.8.15 processes a pong only inside Conn.Read, and the
// stream's one reader applies each INPUT frame before it reads again. A paste
// is one tmux send-keys per chunk, each bounded at 10s, so applying one frame
// can outlast the keepalive interval. While it is applied the peer's pong sits
// unread and the ping times out. That delay is the server's, and it must never
// count as the client's missed keepalive.

// slowInputPTYBinding parks the INPUT "slow" for hold, the way a stalled tmux
// or a chunked paste parks the stream's reader, and applies every other INPUT
// at once. It records what it applied.
type slowInputPTYBinding struct {
	hold    time.Duration
	started chan []byte
	mu      sync.Mutex
	applied []string
}

func (*slowInputPTYBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, errors.New("subscribe is not used by servePTYStream")
}

func (b *slowInputPTYBinding) input(p []byte) error {
	b.started <- append([]byte(nil), p...)
	if string(p) == "slow" {
		time.Sleep(b.hold)
	}
	b.mu.Lock()
	b.applied = append(b.applied, string(p))
	b.mu.Unlock()
	return nil
}

func (*slowInputPTYBinding) resize(uint16, uint16) error { return nil }

func serveSlowInputStream(t *testing.T, binding *slowInputPTYBinding, keepalive time.Duration) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStreamWithKeepalive(binding, &waitingPTYSubscription{}, conn, keepalive)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	conn.SetReadLimit(1 << 20)
	return conn
}

func sendPTYInput(t *testing.T, conn *websocket.Conn, binding *slowInputPTYBinding, p string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agentproto.WriteFrame(ctx, conn, agentproto.InputFrame([]byte(p))); err != nil {
		t.Fatalf("send INPUT %q: %v", p, err)
	}
	select {
	case got := <-binding.started:
		if string(got) != p {
			t.Fatalf("the server began applying %q, want %q", got, p)
		}
	case <-ctx.Done():
		t.Fatalf("the server never began applying INPUT %q", p)
	}
}

// The regression. The peer reads every frame and answers every ping, and the
// server spends five keepalive intervals applying one of its keystrokes. The
// subscriber must still be attached afterwards, and its next keystroke must
// still apply.
func TestServePTYStreamKeepaliveKeepsSubscriberAliveThroughASlowInput(t *testing.T) {
	const keepalive = 400 * time.Millisecond
	binding := &slowInputPTYBinding{hold: 5 * keepalive, started: make(chan []byte, 4)}
	conn := serveSlowInputStream(t, binding, keepalive)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls := &liveSubscriber{conn: conn, ctx: ctx}
	go ls.readPump()

	sendPTYInput(t, conn, binding, "slow")
	// Outlast the input, then let several more pings go by after it.
	time.Sleep(binding.hold + 3*keepalive)
	ls.mu.Lock()
	closed := ls.closed
	ls.mu.Unlock()
	if closed {
		t.Fatalf("the keepalive dropped a subscriber that answered every ping, because the server took %s to apply its input (#7105)", binding.hold)
	}

	sendPTYInput(t, conn, binding, "next")
	deadline := time.Now().Add(5 * time.Second)
	for {
		binding.mu.Lock()
		applied := append([]string(nil), binding.applied...)
		binding.mu.Unlock()
		if len(applied) == 2 {
			if applied[0] != "slow" || applied[1] != "next" {
				t.Fatalf("applied %q, want [slow next]", applied)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the keystroke after the slow input never applied; applied %q", applied)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ls.mu.Lock()
	closed = ls.closed
	ls.mu.Unlock()
	if closed {
		t.Fatal("the subscriber was dropped after the slow input")
	}
}

// The keepalive still drops a peer that has stopped answering, slow input or
// not: once the server is back reading, a missed pong is the peer's again. This
// peer sends one slow keystroke and never reads, so it never answers a ping.
func TestServePTYStreamKeepaliveDropsASilentPeerOnceASlowInputEnds(t *testing.T) {
	const keepalive = 300 * time.Millisecond
	binding := &slowInputPTYBinding{hold: 4 * keepalive, started: make(chan []byte, 4)}
	conn := serveSlowInputStream(t, binding, keepalive)

	sendPTYInput(t, conn, binding, "slow")
	// The input, then a few pings the peer leaves unanswered.
	time.Sleep(binding.hold + 4*keepalive)

	// Reading now drains the hello and then meets the server's close: the
	// keepalive dropped the peer, and the stream closed normally.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			t.Fatalf("the keepalive never dropped a peer that answers no ping: %v", err)
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure {
			t.Fatalf("read after the drop = %v (close status %v), want a normal closure", err, got)
		}
		return
	}
}
