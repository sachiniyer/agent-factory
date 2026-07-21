package daemon

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon's events plane (#1592 Phase 2 PR5, §4.3): a WebSocket/JSON fan-out
// of session and task state changes served at GET /v1/events, so a client can
// replace Snapshot polling with push. Every mutation the daemon owns publishes an
// agentproto.Event to the hub; each connected client receives the stream.
//
// The hub lives on the Manager because BOTH transports (the net/rpc control
// socket and the HTTP server) mutate through the same Manager, so a single hub
// there captures every mutation regardless of which transport drove it. Payloads
// follow agentproto's contract: a session.* event carries a marshaled
// session.InstanceData, a task.* event a marshaled task.Task.
//
// The TUI consumes /v1/events for live updates (#1592 Phase 2 PR6). The route is
// threaded through the auth/CORS seam and covered by the WS harness.

// eventsBufferSize bounds one subscriber's pending-event queue. A subscriber that
// falls this far behind is disconnected (non-blocking publish) rather than
// stalling the mutation that published it. Reconnect triggers the client's
// authoritative Snapshot, so overflow is observable and self-healing instead of
// leaving an open connection with silently stale state.
const eventsBufferSize = 256

// eventsHub is the Manager-owned fan-out of state-change events to every
// connected /v1/events subscriber. Non-blocking on publish (disconnect-slow), so
// no subscriber can back-pressure a daemon mutation.
type eventsHub struct {
	mu   sync.Mutex
	subs map[uint64]chan agentproto.Event
	next uint64
}

func newEventsHub() *eventsHub {
	return &eventsHub{subs: make(map[uint64]chan agentproto.Event)}
}

// subscribe registers a subscriber and returns its id and receive channel.
func (h *eventsHub) subscribe() (uint64, <-chan agentproto.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	ch := make(chan agentproto.Event, eventsBufferSize)
	h.subs[id] = ch
	return id, ch
}

// unsubscribe removes a subscriber.
func (h *eventsHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// publish fans an event to every subscriber. A full buffer removes that
// subscriber and closes its channel (never blocking the mutation that published
// it), making the client's existing reconnect/resync path authoritative.
func (h *eventsHub) publish(ev agentproto.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: close it rather than leave the client with a
			// silently stale open connection. Reconnect causes a Snapshot.
			delete(h.subs, id)
			close(ch)
		}
	}
}

// publishEvent marshals payload into an agentproto.Event and fans it out. A nil
// manager or hub (some test control servers) is a no-op; a marshal error is
// logged and swallowed — an events-plane failure must never break a mutation.
func (m *Manager) publishEvent(t agentproto.EventType, payload any) {
	if m == nil || m.events == nil {
		return
	}
	ev, err := agentproto.NewEvent(t, payload)
	if err != nil {
		log.WarningLog.Printf("events plane: marshal %s event: %v", t, err)
		return
	}
	m.events.publish(ev)
}

// eventsHandler upgrades GET /v1/events to a WebSocket and streams state-change
// events to the client until it disconnects.
//
// Deliberately NOT gated on requireManagerReady, unlike the stream routes (#2109)
// and the web-tab proxy (#1878): it resolves no session and reads no instance
// state — it only subscribes to the hub — so it cannot build an instance off disk
// for the restore to orphan, which is the whole hazard that gate exists for. A
// client that connects mid-warm-up simply starts receiving events as the restore
// publishes them, which is what a client watching a daemon come up wants.
func (cs *controlServer) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil || cs.manager.events == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no events hub"))
		return
	}
	// Permissive origin on the unix socket now (§4.4); Phase 3 tightens it.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	serveEvents(cs.manager.events, conn)
}

// serveEvents streams the hub's events to one connection: a writer loop draining
// the subscriber channel, a reader loop that detects the client's disconnect, and
// a keepalive pinger that drops a dead subscriber without side effects.
func serveEvents(hub *eventsHub, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id, ch := hub.subscribe()
	defer hub.unsubscribe(id)

	var wg sync.WaitGroup
	wg.Add(2)
	// Reader: the events plane is server→client only, so any inbound frame or
	// read error just signals disconnect.
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	go func() { defer wg.Done(); defer cancel(); keepalivePTY(ctx, conn) }()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			wg.Wait()
			return
		case ev, ok := <-ch:
			if !ok {
				_ = conn.Close(websocket.StatusPolicyViolation, "events subscriber fell behind; reconnecting")
				wg.Wait()
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := agentproto.WriteControl(wctx, conn, ev)
			wcancel()
			if err != nil {
				cancel()
				_ = conn.Close(websocket.StatusNormalClosure, "")
				wg.Wait()
				return
			}
		}
	}
}
