package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// TestEventsHubFanoutAndDisconnectSlow pins the hub contract: an event fans out
// to every subscriber, and a subscriber whose bounded buffer is full is
// disconnected rather than blocking publish (so a slow client can never
// back-pressure a daemon mutation).
func TestEventsHubFanoutAndDisconnectSlow(t *testing.T) {
	hub := newEventsHub()
	_, a := hub.subscribe()
	_, b := hub.subscribe()

	ev, err := agentproto.NewEvent(agentproto.EventSessionUpdated, session.InstanceData{Title: "x"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	hub.publish(ev)
	for _, ch := range []<-chan agentproto.Event{a, b} {
		select {
		case got := <-ch:
			if got.Type != agentproto.EventSessionUpdated {
				t.Fatalf("event type = %q, want session.updated", got.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive the published event")
		}
	}

	// Overfill one subscriber's buffer: publish must not block
	// (disconnect-slow).
	done := make(chan struct{})
	go func() {
		for i := 0; i < eventsBufferSize*3; i++ {
			hub.publish(ev)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full subscriber buffer (disconnect-slow broken)")
	}
}

// TestEventsHubOverflowDisconnectsSubscriber is the server half of the web
// client's reconnect/resync invariant (#2274): once a subscriber misses an
// event, keeping its stream open would leave its projection stale forever. The
// hub must evict it and close its event channel so serveEvents can close the
// WebSocket; EventStream then reconnects and requests an authoritative Snapshot.
func TestEventsHubOverflowDisconnectsSubscriber(t *testing.T) {
	hub := newEventsHub()
	id, ch := hub.subscribe()

	ev, err := agentproto.NewEvent(agentproto.EventSessionUpdated, session.InstanceData{Title: "overflow"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	for i := 0; i < eventsBufferSize; i++ {
		hub.publish(ev)
	}

	// This mutation cannot fit. It must make the gap observable instead of
	// silently dropping the event while leaving the subscriber live.
	hub.publish(ev)

	hub.mu.Lock()
	_, live := hub.subs[id]
	hub.mu.Unlock()
	if live {
		t.Error("overflowed subscriber remains registered")
	}

	for i := 0; i < eventsBufferSize; i++ {
		<-ch
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("overflowed subscriber received an event after its buffer drained")
		}
	case <-time.After(time.Second):
		t.Fatal("overflowed subscriber channel stayed open, so its WebSocket cannot reconnect/resync")
	}
}

// TestServeEventsOverflowClosesWebSocket pins the transport half of #2274. The
// hub test above proves saturation emits the overflow signal; this test proves
// serveEvents turns that signal into a retryable close, which is what the web
// EventStream observes before reconnecting and calling onResync.
func TestServeEventsOverflowClosesWebSocket(t *testing.T) {
	hub := newEventsHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		serveEvents(hub, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	var overflow chan struct{}
	deadline := time.Now().Add(time.Second)
	for overflow == nil && time.Now().Before(deadline) {
		hub.mu.Lock()
		for _, sub := range hub.subs {
			overflow = sub.overflow
			break
		}
		hub.mu.Unlock()
		if overflow == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if overflow == nil {
		t.Fatal("serveEvents did not register its subscriber")
	}

	// publish closes this exact signal when the bounded event channel fills.
	close(overflow)
	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusTryAgainLater {
		t.Fatalf("overflow close status = %v (err %v), want %v", got, err, websocket.StatusTryAgainLater)
	}
}

// TestServeEventsDelivers proves the /v1/events serving path end to end over a
// real WebSocket: a published event reaches a connected client.
func TestServeEventsDelivers(t *testing.T) {
	hub := newEventsHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		serveEvents(hub, conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	ev, err := agentproto.NewEvent(agentproto.EventTaskCreated, nil)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	// Publish repeatedly to absorb the subscribe/connect race, until the client
	// reads one event or the deadline trips.
	got := make(chan agentproto.MessageType, 1)
	go func() {
		msg, rerr := agentproto.ReadMessage(ctx, conn)
		if rerr != nil {
			return
		}
		if typ, terr := agentproto.MessageTypeOf(msg.Text); terr == nil {
			got <- typ
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case typ := <-got:
			if string(typ) != string(agentproto.EventTaskCreated) {
				t.Fatalf("event type = %q, want task.created", typ)
			}
			return
		case <-ticker.C:
			hub.publish(ev)
		case <-ctx.Done():
			t.Fatal("client never received a published event")
		}
	}
}
