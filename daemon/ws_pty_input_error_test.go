package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

type rejectingPTYInputBinding struct {
	err         error
	called      chan []byte
	started     chan struct{}
	startedOnce sync.Once
	release     <-chan struct{}
}

func (*rejectingPTYInputBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, errors.New("subscribe is not used by servePTYStream")
}

func (b *rejectingPTYInputBinding) input(p []byte) error {
	b.called <- append([]byte(nil), p...)
	if b.started != nil {
		b.startedOnce.Do(func() { close(b.started) })
	}
	if b.release != nil {
		<-b.release
	}
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

type teardownPTYSubscription struct {
	inputStarted <-chan struct{}
}

func (s *teardownPTYSubscription) NextEvent(ctx context.Context) (session.PTYEvent, error) {
	select {
	case <-s.inputStarted:
		return session.PTYEvent{}, io.EOF
	case <-ctx.Done():
		return session.PTYEvent{}, ctx.Err()
	}
}

func (*teardownPTYSubscription) Seq() session.Seq { return 0 }
func (*teardownPTYSubscription) Close() error     { return nil }

type endedPTYSubscription struct{}

func (*endedPTYSubscription) NextEvent(context.Context) (session.PTYEvent, error) {
	return session.PTYEvent{}, io.EOF
}

func (*endedPTYSubscription) Seq() session.Seq { return 0 }
func (*endedPTYSubscription) Close() error     { return nil }

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

	// An input failure now emits a MsgInputNack (carrying the applied byte count)
	// as the final in-band frame BEFORE the close, so the client knows how much
	// of the failing frame to retain for re-send. The stub binding returns a
	// plain error with no PartialInputError accounting, so the nack reports 0
	// applied — the client must re-send the whole frame.
	nack, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read input nack: %v", err)
	}
	if nack.Binary {
		t.Fatalf("input nack = binary %+v, want text MsgInputNack", nack.Frame)
	}
	if got, err := agentproto.MessageTypeOf(nack.Text); err != nil || got != agentproto.MsgInputNack {
		t.Fatalf("input nack type = %q (err %v), want %q", got, err, agentproto.MsgInputNack)
	}
	var nm agentproto.InputNackMessage
	if err := json.Unmarshal(nack.Text, &nm); err != nil {
		t.Fatalf("unmarshal input nack: %v", err)
	}
	if nm.Applied != 0 {
		t.Fatalf("input nack applied = %d, want 0 (stub binding reports no partial accounting)", nm.Applied)
	}

	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("input failure close status = %v (err %v), want %v", got, err, websocket.StatusInternalError)
	}
}

// TestServePTYStreamTeardownWaitsForInputError proves session teardown cannot
// commit to a normal close while an input delivery is still in flight. It drives
// the ordering from the exit frame the client observes rather than a wall-clock
// scheduling window.
func TestServePTYStreamTeardownWaitsForInputError(t *testing.T) {
	inputStarted := make(chan struct{})
	releaseInput := make(chan struct{})
	binding := &rejectingPTYInputBinding{
		err:     errors.New("tmux rejected input during teardown"),
		called:  make(chan []byte, 1),
		started: inputStarted,
		release: releaseInput,
	}
	sub := &teardownPTYSubscription{inputStarted: inputStarted}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStream(binding, sub, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	hello, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read stream hello: %v", err)
	}
	if !hello.Binary || hello.Frame.Op != agentproto.OpHello {
		t.Fatalf("first message = %+v, want OpHello", hello)
	}

	want := []byte("last keys")
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

	exit, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read session exit during input delivery: %v", err)
	}
	if exit.Binary {
		t.Fatalf("message after input = binary %+v, want %v", exit.Frame, agentproto.MsgExit)
	}
	if got, err := agentproto.MessageTypeOf(exit.Text); err != nil || got != agentproto.MsgExit {
		t.Fatalf("message after input type = %v (err %v), want %v", got, err, agentproto.MsgExit)
	}

	close(releaseInput)
	// The session-end exit frame is followed by the input-failure NACK, then the
	// close. The ordering proof (exit before close) is what the test is named for;
	// the nack is the additive accounting the daemon now emits before tearing down.
	nack, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read input nack during teardown: %v", err)
	}
	if got, err := agentproto.MessageTypeOf(nack.Text); err != nil || got != agentproto.MsgInputNack {
		t.Fatalf("post-exit type = %q (err %v), want %q", got, err, agentproto.MsgInputNack)
	}
	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("teardown input failure close status = %v (err %v), want %v", got, err, websocket.StatusInternalError)
	}
}

func TestServePTYStreamTeardownClosesNormally(t *testing.T) {
	binding := &rejectingPTYInputBinding{
		err:    errors.New("input is not expected"),
		called: make(chan []byte, 1),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStream(binding, &endedPTYSubscription{}, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	hello, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read stream hello: %v", err)
	}
	if !hello.Binary || hello.Frame.Op != agentproto.OpHello {
		t.Fatalf("first message = %+v, want OpHello", hello)
	}
	exit, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read session exit: %v", err)
	}
	if exit.Binary {
		t.Fatalf("message after hello = binary %+v, want %v", exit.Frame, agentproto.MsgExit)
	}
	if got, err := agentproto.MessageTypeOf(exit.Text); err != nil || got != agentproto.MsgExit {
		t.Fatalf("message after hello type = %v (err %v), want %v", got, err, agentproto.MsgExit)
	}

	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure {
		t.Fatalf("session teardown close status = %v (err %v), want %v", got, err, websocket.StatusNormalClosure)
	}
}

// TestServePTYStreamInputNackCarriesAppliedPrefix proves the daemon forwards the
// tmux PartialInputError's Applied count to the client in the MsgInputNack, so
// the client re-sends only the unapplied tail. The stub binding returns a
// *PartialInputError (as the real chunked SendRawKeys now does on a mid-frame
// chunk failure); the nack on the wire must carry that exact Applied count.
func TestServePTYStreamInputNackCarriesAppliedPrefix(t *testing.T) {
	binding := &rejectingPTYInputBinding{
		err:    &sessiontmux.PartialInputError{Applied: 8192, Err: errors.New("tmux chunk failed mid-frame")},
		called: make(chan []byte, 1),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStream(binding, &waitingPTYSubscription{}, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	hello, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read stream hello: %v", err)
	}
	if !hello.Binary || hello.Frame.Op != agentproto.OpHello {
		t.Fatalf("first message = %+v, want OpHello", hello)
	}

	want := []byte(strings.Repeat("p", 16384))
	if err := agentproto.WriteFrame(ctx, conn, agentproto.InputFrame(want)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case got := <-binding.called:
		if string(got) != string(want) {
			t.Fatalf("binding input = %d bytes, want %d", len(got), len(want))
		}
	case <-ctx.Done():
		t.Fatalf("binding did not receive input: %v", ctx.Err())
	}

	nack, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read input nack: %v", err)
	}
	if nack.Binary {
		t.Fatalf("nack = binary %+v, want text MsgInputNack", nack.Frame)
	}
	if got, err := agentproto.MessageTypeOf(nack.Text); err != nil || got != agentproto.MsgInputNack {
		t.Fatalf("nack type = %q (err %v), want %q", got, err, agentproto.MsgInputNack)
	}
	var nm agentproto.InputNackMessage
	if err := json.Unmarshal(nack.Text, &nm); err != nil {
		t.Fatalf("unmarshal input nack: %v", err)
	}
	if nm.Applied != 8192 {
		t.Fatalf("nack applied = %d, want 8192 (the PartialInputError count must reach the client)", nm.Applied)
	}

	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("input failure close status = %v (err %v), want %v", got, err, websocket.StatusInternalError)
	}
}

// acceptingPTYInputBinding records each INPUT frame its input() is handed and
// applies them successfully, so the daemon emits a MsgInputAck per frame.
type acceptingPTYInputBinding struct {
	mu     sync.Mutex
	frames [][]byte
}

func (b *acceptingPTYInputBinding) subscribe(session.Seq) (session.PTYSubscription, error) {
	return nil, errors.New("subscribe is not used by servePTYStream")
}
func (b *acceptingPTYInputBinding) input(p []byte) error {
	b.mu.Lock()
	b.frames = append(b.frames, append([]byte(nil), p...))
	b.mu.Unlock()
	return nil
}
func (*acceptingPTYInputBinding) resize(uint16, uint16) error { return nil }
func (b *acceptingPTYInputBinding) received() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, len(b.frames))
	copy(out, b.frames)
	return out
}

// TestServePTYStreamAcksAppliedInput proves every successfully applied INPUT
// frame is confirmed to the client with a MsgInputAck — one ack per frame, in
// arrival order — so a client that trims only on ack never silently drops a
// frame the daemon has not yet applied. Two frames go in; two acks come back.
func TestServePTYStreamAcksAppliedInput(t *testing.T) {
	binding := &acceptingPTYInputBinding{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		servePTYStream(binding, &waitingPTYSubscription{}, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial PTY stream: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	hello, err := agentproto.ReadMessage(ctx, conn)
	if err != nil {
		t.Fatalf("read stream hello: %v", err)
	}
	if !hello.Binary || hello.Frame.Op != agentproto.OpHello {
		t.Fatalf("first message = %+v, want OpHello", hello)
	}

	frames := [][]byte{[]byte("first frame"), []byte("second frame")}
	for _, f := range frames {
		if err := agentproto.WriteFrame(ctx, conn, agentproto.InputFrame(f)); err != nil {
			t.Fatalf("write input: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(binding.received()) < len(frames) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(binding.received()); got < len(frames) {
		t.Fatalf("daemon received %d input frames, want %d", got, len(frames))
	}

	// One ack per applied frame, in order. The ack carries no byte count (the
	// stream is FIFO), so the client pairs each ack with its oldest unconfirmed
	// frame.
	for i := 0; i < len(frames); i++ {
		ack, err := agentproto.ReadMessage(ctx, conn)
		if err != nil {
			t.Fatalf("read input ack %d: %v", i, err)
		}
		if ack.Binary {
			t.Fatalf("ack %d = binary %+v, want text MsgInputAck", i, ack.Frame)
		}
		if got, err := agentproto.MessageTypeOf(ack.Text); err != nil || got != agentproto.MsgInputAck {
			t.Fatalf("ack %d type = %q (err %v), want %q", i, got, err, agentproto.MsgInputAck)
		}
		var am agentproto.InputAckMessage
		if err := json.Unmarshal(ack.Text, &am); err != nil {
			t.Fatalf("unmarshal ack %d: %v", i, err)
		}
	}
}
