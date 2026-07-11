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

// TestEventsHubFanoutAndDropSlow pins the hub contract: an event fans out to
// every subscriber, and a subscriber whose bounded buffer is full drops events
// rather than blocking publish (so a slow client can never back-pressure a daemon
// mutation).
func TestEventsHubFanoutAndDropSlow(t *testing.T) {
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

	// Overfill one subscriber's buffer: publish must not block (drop-slow).
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
		t.Fatal("publish blocked on a full subscriber buffer (drop-slow broken)")
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
