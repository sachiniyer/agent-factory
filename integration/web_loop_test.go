package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
)

// TestWebActionsLoop is the #1592 Phase 5 PR5 play-test in code: it drives the
// EXACT daemon HTTP+WS endpoints the browser client speaks — CreateSession over
// POST /v1/CreateSession, the live rail over the /v1/events WebSocket, the attach
// terminal over /v1/sessions/{id}/stream, SendPrompt, and KillSession — against a
// REAL daemon serving a REAL local tmux session in the container fence. It proves
// the v1 loop end to end (list → attach → type → create/kill) and the two
// server-side correctness fixes this PR folds in:
//
//   - the delete-class events carry the STABLE id (session.killed here), so a
//     browser rail keys off the id, not the collision-prone title;
//   - killing the attached session settles its stream with a MsgExit control frame
//     (the browser terminal shows "exited" and stops reconnecting) rather than the
//     bare socket close a droppable network drop is indistinguishable from.
//
// Run it in the container fence: make test-container GOTESTARGS="./integration
// -run TestWebActionsLoop". The output (-v) is the play-test transcript.
func TestWebActionsLoop(t *testing.T) {
	h := newHarness(t)
	h.startDaemon()

	// --- subscribe to /v1/events, as the web rail does on login ---
	ev := h.dialEvents(t)
	defer ev.close()
	t.Log("STEP 1: subscribed to /v1/events (the live rail)")

	// --- create a session via the modal's CreateSession POST ---
	var created struct {
		Instance struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"instance"`
	}
	err := h.tryHTTPPost("/v1/CreateSession", map[string]any{
		"title_base": "webloop",
		"repo_path":  h.repo,
		"program":    "claude",
	}, &created)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id := created.Instance.ID
	title := created.Instance.Title
	if id == "" || title == "" {
		t.Fatalf("created session missing id/title: %+v", created)
	}
	t.Logf("STEP 2: created session title=%q id=%q via CreateSession", title, id)

	// --- the created event reaches the rail, carrying the id ---
	ev.waitEvent(t, agentproto.EventSessionCreated, func(d eventData) bool {
		return d.ID == id && d.Title == title
	})
	t.Logf("STEP 3: session.created event arrived on the rail carrying id=%q", id)

	// --- attach the terminal to /v1/sessions/{id}/stream and type ---
	term := h.dialWS(t, "/v1/sessions/"+id+"/stream")
	defer term.close()
	term.sendInput(t, []byte("hello-from-web\n"))
	term.waitOutput(t, "hello-from-web")
	t.Log("STEP 4: attached the terminal by id and typed — the pane echoed it")

	// --- send a prompt via SendPrompt, mirroring the send-prompt modal ---
	if err := h.tryHTTPPost("/v1/SendPrompt", map[string]any{
		"title":   title,
		"repo_id": "",
		"prompt":  "a prompt from the web",
	}, nil); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	term.waitOutput(t, "a prompt from the web")
	t.Log("STEP 5: sent a prompt via SendPrompt — it reached the session")

	// --- kill via KillSession (behind the confirm modal) ---
	if err := h.tryHTTPPost("/v1/KillSession", map[string]any{
		"title":   title,
		"repo_id": "",
	}, nil); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	t.Log("STEP 6: killed the session via KillSession (the confirm modal's action)")

	// --- the attached terminal settles with MsgExit (no reconnect loop) ---
	waitUntil(t, 5*time.Second, "attached terminal received MsgExit on kill", func() bool {
		return term.sawExit()
	})
	t.Log("STEP 7: the attached terminal received MsgExit → settles to \"exited\", stops reconnecting")

	// --- the killed event carries the id, so the rail removes exactly this row ---
	ev.waitEvent(t, agentproto.EventSessionKilled, func(d eventData) bool {
		return d.ID == id && d.Title == title
	})
	t.Logf("STEP 8: session.killed event carried id=%q → the rail drops exactly this row, live", id)
}

// eventData is the subset of the session.* event payload the play-test asserts on.
type eventData struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// eventsClient is a /v1/events subscriber that accumulates decoded session.* events.
type eventsClient struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	got    chan agentproto.Event
}

// dialEvents opens the events plane and pumps decoded events into a channel.
func (h *harness) dialEvents(t *testing.T) *eventsClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "ws://unix/v1/events", &websocket.DialOptions{HTTPClient: h.httpClient()})
	if err != nil {
		cancel()
		t.Fatalf("dial /v1/events: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	c := &eventsClient{conn: conn, ctx: ctx, cancel: cancel, got: make(chan agentproto.Event, 256)}
	go c.pump()
	return c
}

func (c *eventsClient) pump() {
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var e agentproto.Event
		if json.Unmarshal(data, &e) == nil {
			select {
			case c.got <- e:
			default:
			}
		}
	}
}

// waitEvent blocks until an event of the given type whose payload satisfies match
// arrives, failing the test on timeout.
func (c *eventsClient) waitEvent(t *testing.T, want agentproto.EventType, match func(eventData) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-c.got:
			if e.Type != want {
				continue
			}
			var d eventData
			if len(e.Data) > 0 {
				_ = json.Unmarshal(e.Data, &d)
			}
			if match(d) {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event matching the predicate within the deadline", want)
			return
		}
	}
}

func (c *eventsClient) close() {
	c.cancel()
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}
