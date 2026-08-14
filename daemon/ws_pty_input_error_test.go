package daemon

import (
	"bytes"
	"context"
	"errors"
	"net"
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

type rejectingPTYInputBinding struct {
	err    error
	called chan []byte
}

func (*rejectingPTYInputBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, errors.New("subscribe is not used by servePTYStream")
}

func (b *rejectingPTYInputBinding) input(p []byte) error {
	b.called <- append([]byte(nil), p...)
	return b.err
}

func (*rejectingPTYInputBinding) resize(uint16, uint16) error { return nil }

type waitingPTYSubscription struct{}

func (*waitingPTYSubscription) NextEvent(ctx context.Context) (session.PTYEvent, error) {
	<-ctx.Done()
	return session.PTYEvent{}, ctx.Err()
}

func (*waitingPTYSubscription) Seq() session.Seq { return 0 }
func (*waitingPTYSubscription) Close() error     { return nil }

type delayedPTYWriteConn struct {
	net.Conn
	mu             sync.Mutex
	upgraded       bool
	delayOnce      sync.Once
	releaseOnce    sync.Once
	frameWritten   chan struct{}
	releaseFrame   chan struct{}
	transportClose chan struct{}
	closeOnce      sync.Once
}

func (c *delayedPTYWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)

	c.mu.Lock()
	upgraded := c.upgraded
	if bytes.HasPrefix(p[:n], []byte("HTTP/1.1 101")) {
		c.upgraded = true
	}
	c.mu.Unlock()

	if upgraded && n > 0 {
		c.delayOnce.Do(func() {
			close(c.frameWritten)
			<-c.releaseFrame
		})
	}
	return n, err
}

func (c *delayedPTYWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.transportClose) })
	return c.Conn.Close()
}

func (c *delayedPTYWriteConn) release() {
	c.releaseOnce.Do(func() { close(c.releaseFrame) })
}

type delayedPTYWriteListener struct {
	net.Listener
	accepted chan *delayedPTYWriteConn
}

func (l *delayedPTYWriteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	delayed := &delayedPTYWriteConn{
		Conn:           conn,
		frameWritten:   make(chan struct{}),
		releaseFrame:   make(chan struct{}),
		transportClose: make(chan struct{}),
	}
	l.accepted <- delayed
	return delayed, nil
}

// TestServePTYStreamInputErrorClosesWebSocket proves a failed input delivery is
// observable to the web terminal. The reconnecting client can hold subsequent
// keys only if the server closes this failed stream instead of silently reading
// and dropping every later OpInput frame (#3138).
func TestServePTYStreamInputErrorClosesWebSocket(t *testing.T) {
	binding := &rejectingPTYInputBinding{
		err:    errors.New("tmux rejected input"),
		called: make(chan []byte, 1),
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStream(binding, &waitingPTYSubscription{}, conn)
	}))
	listener := &delayedPTYWriteListener{Listener: srv.Listener, accepted: make(chan *delayedPTYWriteConn, 1)}
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	serverConn := <-listener.accepted
	defer serverConn.release()
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	select {
	case <-serverConn.frameWritten:
	case <-ctx.Done():
		t.Fatalf("server did not write stream hello: %v", ctx.Err())
	}
	hello, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read stream hello: %v", err)
	}
	if !hello.Binary || hello.Frame.Op != agentproto.OpHello {
		t.Fatalf("first message = %+v, want OpHello", hello)
	}

	want := []byte("typed")
	if err := agentproto.WriteFrame(ctx, conn, agentproto.InputFrame(want)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case got := <-binding.called:
		if string(got) != string(want) {
			t.Fatalf("binding input = %q, want %q", got, want)
		}
	case <-ctx.Done():
		t.Fatalf("binding did not receive input: %v", ctx.Err())
	}

	// Hold the completed hello write inside websocket.Write while the input
	// failure cancels the stream. coder/websocket closes the transport when a
	// context passed to an in-flight write is cancelled; the explicit close
	// frame must remain possible even in this scheduling window.
	select {
	case <-serverConn.transportClose:
	case <-time.After(100 * time.Millisecond):
	}
	serverConn.release()

	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("input failure close status = %v (err %v), want %v", got, err, websocket.StatusInternalError)
	}
}
