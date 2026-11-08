package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/session"
)

// Tests for the keepalive pinger's drop-vs-retry logic. The bug
// (daemon/ws_pty.go keepalivePTY): coder/websocket v1.8.15's Conn.Ping wraps the
// caller's context with a hidden 5s write-path deadline and must acquire the same
// write mutex a data-frame Write holds for its whole duration, so a ping issued
// while a slow-but-healthy data write is in flight fails with "failed to acquire
// lock" even though the peer is alive and reading. The pre-fix code dropped the
// subscriber on ANY ping error, churning slow-but-healthy clients. These tests
// pin the fix: only a missed pong (or a closed connection) drops; a write-path
// failure is retried.

// withKeepalive sets wsKeepaliveInterval for the duration of fn so a keepalive
// loop ticks in test time, then restores it. wsKeepaliveInterval is a package var
// ("var so tests can shrink it"), but these tests do not run in parallel so the
// save/restore is race-free.
func withKeepalive(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	prev := wsKeepaliveInterval
	wsKeepaliveInterval = d
	t.Cleanup(func() { wsKeepaliveInterval = prev })
	fn()
}

func TestClassifyPingError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want keepaliveDecision
	}{
		{"nil continues", nil, keepaliveContinue},
		{"missed pong drops",
			fmt.Errorf("failed to wait for pong: %w", context.DeadlineExceeded), keepaliveDrop},
		{"missed pong wrapped by ping drops",
			fmt.Errorf("failed to ping: failed to wait for pong: %w", context.DeadlineExceeded), keepaliveDrop},
		{"closed connection drops", net.ErrClosed, keepaliveDrop},
		{"closed connection wrapped by ping drops",
			fmt.Errorf("failed to ping: %w", net.ErrClosed), keepaliveDrop},
		{"closed connection wrapped by write-control drops",
			fmt.Errorf("failed to ping: failed to write control frame opPing: %w", net.ErrClosed), keepaliveDrop},
		// The bug case: the library's hidden 5s write-lock timeout against a
		// slow-but-healthy data-frame write. This MUST NOT drop.
		{"write-lock timeout retries",
			fmt.Errorf("failed to ping: failed to write control frame opPing: failed to acquire lock: %w", context.DeadlineExceeded),
			keepaliveRetry},
		// A raw deadline exceeded without the pong-wait string is the slow-write
		// path (write took >5s); it must NOT be treated as a missed pong.
		{"bare deadline exceeded (slow write) retries",
			context.DeadlineExceeded, keepaliveRetry},
		// A generic write I/O failure is not a missed pong either; the writer
		// goroutine's wsWriteTimeout handles a wedged client.
		{"generic write error retries",
			fmt.Errorf("failed to ping: failed to write control frame opPing: failed to write frame: write: broken pipe"),
			keepaliveRetry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPingError(tt.err); got != tt.want {
				t.Errorf("classifyPingError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestKeepaliveLoopDropsOnMissedPong: a ping written but never answered must drop
// the subscriber after a single tick — the documented "dead subscriber" behavior
// must survive the fix.
func TestKeepaliveLoopDropsOnMissedPong(t *testing.T) {
	withKeepalive(t, 20*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		calls := make(chan struct{}, 16)
		ping := func(context.Context) error {
			select {
			case calls <- struct{}{}:
			default:
			}
			return fmt.Errorf("failed to wait for pong: %w", context.DeadlineExceeded)
		}
		done := make(chan struct{})
		go func() { keepaliveLoop(ctx, ping); close(done) }()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("keepaliveLoop did not drop on a missed pong")
		}
		if n := len(calls); n != 1 {
			t.Fatalf("expected exactly one ping before dropping, got %d", n)
		}
	})
}

// TestKeepaliveLoopRetriesWriteLockTimeout: the bug case. A ping whose WRITE
// keeps failing with the library's write-lock timeout must NOT drop the
// subscriber; the loop keeps retrying across ticks until the peer's egress
// frees up (or the writer's wsWriteTimeout drops a truly wedged client).
func TestKeepaliveLoopRetriesWriteLockTimeout(t *testing.T) {
	withKeepalive(t, 20*time.Millisecond, func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		calls := make(chan struct{}, 1024)
		ping := func(context.Context) error {
			select {
			case calls <- struct{}{}:
			default:
			}
			return fmt.Errorf("failed to acquire lock: %w", context.DeadlineExceeded)
		}
		done := make(chan struct{})
		go func() { keepaliveLoop(ctx, ping); close(done) }()

		// Across several ticks the loop must keep retrying, not drop.
		select {
		case <-done:
			t.Fatal("keepaliveLoop dropped on a write-lock timeout; should retry")
		case <-time.After(150 * time.Millisecond):
		}
		cancel()
		<-done
		// No more sends after <-done; len is race-free.
		if n := len(calls); n < 3 {
			t.Fatalf("expected at least 3 ping retries across ticks, got %d", n)
		}
	})
}

// TestKeepaliveLoopDropsOnClosedConnection: a ping error that wraps net.ErrClosed
// means the transport is gone; the keepalive must drop (and let the stream
// teardown close the socket) rather than retry forever.
func TestKeepaliveLoopDropsOnClosedConnection(t *testing.T) {
	withKeepalive(t, 20*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan struct{})
		ping := func(context.Context) error { return net.ErrClosed }
		go func() { keepaliveLoop(ctx, ping); close(done) }()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("keepaliveLoop did not drop on a closed connection")
		}
	})
}

// TestKeepaliveLoopContinuesOnSuccessThenDropsOnMissedPong: a run of successful
// pongs must not mask a later genuine dead-peer signal — the loop keeps going
// while the peer answers and drops as soon as it stops.
func TestKeepaliveLoopContinuesOnSuccessThenDropsOnMissedPong(t *testing.T) {
	withKeepalive(t, 20*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var n int32
		ping := func(context.Context) error {
			if atomic.AddInt32(&n, 1) <= 3 {
				return nil // first three pongs arrive
			}
			return fmt.Errorf("failed to wait for pong: %w", context.DeadlineExceeded)
		}
		done := make(chan struct{})
		go func() { keepaliveLoop(ctx, ping); close(done) }()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("keepaliveLoop did not drop after a later missed pong")
		}
		if got := atomic.LoadInt32(&n); got < 4 {
			t.Fatalf("expected at least 4 pings (3 ok + 1 dead), got %d", got)
		}
	})
}

// TestKeepaliveLoopStopsOnContextDoneBeforeFirstTick: if the stream is torn down
// before the first tick (e.g. a session end cancels ctx during the initial
// backlog replay), the keepalive must exit without ever pinging.
func TestKeepaliveLoopStopsOnContextDoneBeforeFirstTick(t *testing.T) {
	withKeepalive(t, time.Hour, func() {
		ctx, cancel := context.WithCancel(context.Background())
		ping := func(context.Context) error {
			t.Fatal("ping must not be called before the first tick")
			return nil
		}
		done := make(chan struct{})
		go func() { keepaliveLoop(ctx, ping); close(done) }()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("keepaliveLoop did not stop on context cancellation")
		}
	})
}

// --- end-to-end: servePTYStream with a slow-but-healthy subscriber ---

// noopPTYInputBinding is a ptyTabBinding whose input/resize are no-ops; subscribe
// is unused by servePTYStream (the handler resolves the subscription before
// calling it).
type noopPTYInputBinding struct{}

func (*noopPTYInputBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, errors.New("subscribe is not used by servePTYStream")
}
func (*noopPTYInputBinding) input([]byte) error          { return nil }
func (*noopPTYInputBinding) resize(uint16, uint16) error { return nil }

// singleBigEventPTYSubscription emits one 256KB PTYData event (the broker's
// lagging-subscriber span→head copy, which becomes a single large Write holding
// the websocket write mutex), then blocks until ctx ends. It never returns io.EOF
// so a live subscriber would stay attached indefinitely.
type singleBigEventPTYSubscription struct {
	once sync.Once
}

func (s *singleBigEventPTYSubscription) NextEvent(ctx context.Context) (session.PTYEvent, error) {
	emitted := false
	s.once.Do(func() { emitted = true })
	if emitted {
		return session.PTYEvent{Kind: session.PTYData, Data: bytes.Repeat([]byte("x"), 256<<10)}, nil
	}
	<-ctx.Done()
	return session.PTYEvent{}, ctx.Err()
}

func (*singleBigEventPTYSubscription) Seq() session.Seq { return 0 }
func (*singleBigEventPTYSubscription) Close() error     { return nil }

// slowLinkPTYConn wraps a server-side net.Conn and holds the FIRST large
// post-upgrade Write for `hold`, simulating a slow link: the websocket write
// mutex stays held for that whole frame write, so a keepalive ping issued during
// it contends for the mutex and hits coder/websocket's hidden 5s write-path cap.
type slowLinkPTYConn struct {
	net.Conn
	mu            sync.Mutex
	upgraded      bool
	throttleOnce  sync.Once
	hold          time.Duration
	writeStarted  chan struct{}
	writeReleased chan struct{}
}

func (c *slowLinkPTYConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.upgraded && bytes.HasPrefix(p, []byte("HTTP/1.1 101")) {
		c.upgraded = true
	}
	upgraded := c.upgraded
	c.mu.Unlock()
	if upgraded && len(p) >= 4096 {
		c.throttleOnce.Do(func() {
			close(c.writeStarted)
			time.Sleep(c.hold)
			close(c.writeReleased)
		})
	}
	return c.Conn.Write(p)
}

type slowLinkPTYListener struct {
	net.Listener
	accepted chan *slowLinkPTYConn
}

func (l *slowLinkPTYListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &slowLinkPTYConn{
		Conn:          c,
		hold:          6500 * time.Millisecond,
		writeStarted:  make(chan struct{}),
		writeReleased: make(chan struct{}),
	}
	l.accepted <- sc
	return sc, nil
}

// liveSubscriber reads every byte the server sends and answers keepalive pings
// automatically (coder/websocket handles pongs inside Read). It is the
// slow-but-healthy subscriber the buggy keepalive used to drop.
type liveSubscriber struct {
	conn   *websocket.Conn
	ctx    context.Context
	mu     sync.Mutex
	bytes  int
	closed bool
}

func (ls *liveSubscriber) readPump() {
	for {
		_, data, err := ls.conn.Read(ls.ctx)
		if err != nil {
			ls.mu.Lock()
			ls.closed = true
			ls.mu.Unlock()
			return
		}
		ls.mu.Lock()
		ls.bytes += len(data)
		ls.mu.Unlock()
	}
}

// TestServePTYStreamKeepaliveKeepsSlowSubscriberAlive is the end-to-end
// regression for the bug. It drives the real servePTYStream path against a
// rate-limited connection: the server ships one 256KB PTYData frame whose
// underlying Write is held 6.5s (past the library's 5s ping write-path cap), so
// the keepalive ping that fires during that hold fails with a write-lock
// timeout. With the bug, that failure tore down the subscriber; with the fix the
// ping is retried, the frame completes, the next ping succeeds, and the
// slow-but-healthy subscriber stays attached.
func TestServePTYStreamKeepaliveKeepsSlowSubscriberAlive(t *testing.T) {
	// A 1s keepalive so the first ping lands well inside the 6.5s slow write.
	withKeepalive(t, 1*time.Second, func() {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			servePTYStream(&noopPTYInputBinding{}, &singleBigEventPTYSubscription{}, conn)
		}))
		listener := &slowLinkPTYListener{Listener: srv.Listener, accepted: make(chan *slowLinkPTYConn, 1)}
		srv.Listener = listener
		srv.Start()
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		clientConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial PTY stream: %v", err)
		}
		defer func() { _ = clientConn.Close(websocket.StatusNormalClosure, "") }()
		clientConn.SetReadLimit(1 << 20)
		serverConn := <-listener.accepted

		ls := &liveSubscriber{conn: clientConn, ctx: ctx}
		go ls.readPump()

		// Confirm the slow write engaged (the keepalive's ping target is in
		// flight and holding the write mutex).
		select {
		case <-serverConn.writeStarted:
		case <-ctx.Done():
			t.Fatalf("slow data write never engaged: %v", ctx.Err())
		}
		// Wait out the hold. During it the first keepalive ping (at t=1s) is
		// blocked on the write mutex and fails with a write-lock timeout at the
		// library's 5s cap (~t=6s). With the bug the subscriber would be torn
		// down around when this hold releases; with the fix the ping is retried.
		select {
		case <-serverConn.writeReleased:
		case <-ctx.Done():
			t.Fatalf("slow data write never completed: %v", ctx.Err())
		}
		// Let the next ping land and succeed now that the mutex is free.
		time.Sleep(2 * time.Second)

		ls.mu.Lock()
		closed := ls.closed
		received := ls.bytes
		ls.mu.Unlock()
		if closed {
			t.Fatal("keepalive dropped a slow-but-healthy subscriber during sustained output (the bug)")
		}
		if received < 256<<10 {
			t.Fatalf("subscriber received %d bytes, want >= 256KB (the slow frame completed)", received)
		}
	})
}

// TestServePTYStreamKeepaliveDropsDeadSubscriber pins the no-regression: a
// subscriber that never answers a pong (it stops reading after the initial
// frames) is still dropped by the keepalive, so the session is not left
// attached to a dead peer. Dead-peer drop is the keepalive's documented job and
// the fix must preserve it.
func TestServePTYStreamKeepaliveDropsDeadSubscriber(t *testing.T) {
	withKeepalive(t, 300*time.Millisecond, func() {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			servePTYStream(&noopPTYInputBinding{}, &waitingPTYSubscription{}, conn)
		}))
		srv.Start()
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		clientConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial PTY stream: %v", err)
		}
		defer func() { _ = clientConn.Close(websocket.StatusNormalClosure, "") }()
		clientConn.SetReadLimit(1 << 20)

		// Drain the leading OpHello so the server is not still mid-handshake when
		// the keepalive fires. After this the client stops reading, so pings are
		// written but never answered with a pong → "failed to wait for pong" → drop.
		if _, _, err := clientConn.Read(ctx); err != nil {
			t.Fatalf("read stream hello: %v", err)
		}
		// Wait out several keepalive cycles (300ms ping + 300ms pong timeout).
		time.Sleep(1500 * time.Millisecond)
		// The server closed the dead subscriber: its next read now errors.
		readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer readCancel()
		if _, _, err := clientConn.Read(readCtx); err == nil {
			t.Fatal("dead subscriber was not dropped by the keepalive")
		}
	})
}
