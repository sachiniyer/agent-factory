package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The daemon's WebSocket PTY stream broker (#1592 Phase 2 PR5) — the data plane
// the control plane (#1029 REST mirror) sits beside. It exposes two routes:
//
//	GET /v1/sessions/{id}/stream?since=<seq>  — the live PTY stream
//	GET /v1/sessions/{id}/stream-info         — where that stream is reachable
//
// The stream upgrades to a WebSocket (agentproto's coder/websocket binding) and
// fans the session's raw PTY output to every subscriber from the local
// agent-server's bounded ring buffer; `?since=<seq>` replays the tail a
// reconnecting client missed. It is MULTI-WRITER with no lease: INPUT and RESIZE
// frames are accepted from ANY subscriber and applied via the agent-server
// (RESIZE is last-resize-wins with an authoritative echo). A WS keepalive ping
// drops a dead subscriber WITHOUT touching the PTY/session — the structural
// reliability win over a shared tmux render client (§6).
//
// The TUI consumes this plane for live panes and full-screen attach (#1592
// Phase 2 PR6/PR7); the routes are threaded through the auth/CORS seam
// (httpserver.go) and exercised by the WS integration harness.

// wsKeepaliveInterval is how often the broker pings each subscriber and how long
// it waits for the pong before dropping that (dead) subscriber. A subscriber
// whose peer stops responding is closed WITHOUT affecting the session or other
// subscribers. Overridable via AF_WS_KEEPALIVE_INTERVAL so the integration
// harness can shorten it (the daemon runs as its own process, so a package var
// alone is not reachable from the harness).
var wsKeepaliveInterval = envDurationOr("AF_WS_KEEPALIVE_INTERVAL", 15*time.Second)

// streamSeqHeader carries the subscription's starting seq on the WS handshake
// response so a client can compute its absolute cursor (startSeq + bytesReceived)
// and reconnect with ?since=<cursor> to replay the gap it missed.
const streamSeqHeader = "X-Af-Stream-Seq"

// wsWriteTimeout bounds a single frame write to a subscriber, so a wedged client
// that has stopped reading cannot block its writer goroutine forever — the write
// fails, the subscriber is dropped, and the session is untouched. var so tests
// can shrink it.
var wsWriteTimeout = 10 * time.Second

// envDurationOr parses a time.Duration from env key, falling back to def when the
// variable is unset or unparseable.
func envDurationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// streamHandler upgrades GET /v1/sessions/{id}/stream to a WebSocket and serves
// the session's PTY stream. Session resolution and Subscribe happen BEFORE the
// upgrade so a missing session or bad cursor returns an HTTP error envelope;
// after websocket.Accept the connection is hijacked and errors close the socket.
func (cs *controlServer) streamHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	// Refuse during warm-up, BEFORE anything reads session state (#2109) — the same
	// gate every state-dependent RPC goes through, and the one webTabProxyHandler
	// was brought under in #1878 for this identical reason: an HTTP route is not
	// net/rpc, so it slips the gating by default.
	//
	// The listener binds long before the restore finishes (#829), and a stale client
	// reconnects the instant the port answers. Resolution runs refreshLocked, which
	// builds instances off disk; RestoreInstances then rebuilds that map wholesale,
	// so the instance already handed to this client stops being the tracked one —
	// requireManagerReady's own comment names it, "throwaway instances from disk
	// that the restore then orphans".
	//
	// It has to fire BEFORE websocket.Accept. Past the upgrade the only refusal
	// available is a close frame, which a client reads as a stream that DROPPED and
	// handles on an entirely different path; a plain 503 is a retry it already
	// understands, and matches every other pre-upgrade error on this route.
	if err := cs.requireManagerReady(); err != nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, err)
		return
	}
	id := r.PathValue("id")
	repoID := r.URL.Query().Get("repo_id")
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	tab, err := parseTab(r.URL.Query().Get("tab"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	as, instance, err := cs.manager.agentServerForStream(id, repoID)
	if err != nil {
		writeHTTPError(w, r, http.StatusNotFound, err)
		return
	}
	// Address the tab by its STABLE id (#1738) when the client supplies ?tab_id=.
	// An id-native runtime resolves the id atomically on EVERY operation — the
	// initial Subscribe and each later Input/Resize — so a reorder/close on another
	// client can never redirect the connection. The ordinal-only remote bridge
	// re-resolves per operation and is safe while that runtime's roster remains
	// immutable; bindTab documents that capability boundary. Without a ?tab_id=
	// (legacy client) the binding pins ?tab=, preserving positional behavior.
	binding, err := cs.bindTab(as, instance, r.URL.Query().Get("tab_id"), tab)
	if err != nil {
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	sub, err := binding.subscribe(since)
	if err != nil {
		// A stale/unknown id is a 404 refusal, never a fall back to a positional tab.
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	// Announce the subscription's starting cursor on the handshake response so the
	// client knows its absolute seq: PTY_OUT frames carry raw bytes with no seq, so
	// a client tracks its position as startSeq + bytesReceived and reconnects with
	// ?since=<that> to replay a gap. Set BEFORE Accept so it rides the 101 response.
	w.Header().Set(streamSeqHeader, strconv.FormatUint(uint64(sub.Seq()), 10))
	// Permissive origin check on the unix socket now (§4.4); Phase 3's CORS policy
	// tightens it for the TCP transport without reshaping this handler.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		_ = sub.Close()
		return // Accept already wrote the error response.
	}
	servePTYStream(binding, sub, conn)
}

// ptyTabBinding is how ONE connection addresses its tab for the whole of its
// life. It exists so the addressing decision — stable id (#1738) vs legacy
// ordinal — is made exactly once, at bind time, instead of being re-derived (and
// re-raced) at each operation. Every op re-resolves through the same binding, so
// the initial Subscribe and every later Input/Resize agree on which tab they mean.
type ptyTabBinding interface {
	subscribe(since session.Seq) (session.PTYSubscription, error)
	input(b []byte) error
	resize(rows, cols uint16) error
}

// idTabBinding addresses the tab by its STABLE id through the agent-server's
// id-native plane. The id is resolved to a tmux target atomically inside each
// operation, so no window exists in which a concurrent close/reorder can shift a
// different tab under this connection. A closed tab yields ErrTabGone — the op is
// refused, never re-pointed at whatever tab took its place.
type idTabBinding struct {
	as    session.TabAddressableServer
	tabID string
}

func (b idTabBinding) subscribe(since session.Seq) (session.PTYSubscription, error) {
	return b.as.SubscribeTab(b.tabID, since)
}
func (b idTabBinding) input(p []byte) error           { return b.as.InputTab(b.tabID, p) }
func (b idTabBinding) resize(rows, cols uint16) error { return b.as.ResizeTab(b.tabID, rows, cols) }

// ordinalTabBinding is the legacy ?tab=<idx> path: without a stable ?tab_id= there
// is nothing to re-resolve, so every op addresses the same fixed ordinal for the
// connection's life (the pre-#1738 positional behavior).
type ordinalTabBinding struct {
	as  session.AgentServer
	tab int
}

func (b ordinalTabBinding) subscribe(since session.Seq) (session.PTYSubscription, error) {
	return b.as.Subscribe(b.tab, since)
}
func (b ordinalTabBinding) input(p []byte) error { return b.as.Input(b.tab, p) }
func (b ordinalTabBinding) resize(rows, cols uint16) error {
	return b.as.Resize(b.tab, rows, cols)
}

// resolvingTabBinding carries an id-addressed connection to a runtime with NO
// id-native plane — the remote agent-server, whose wire protocol is ordinal-shaped
// (its brokers and channels are keyed by index), so the id cannot be pushed all the
// way down the way it can locally.
//
// It re-resolves the id to a CURRENT ordinal on every operation rather than pinning
// the one it saw at bind time. Pinning would mean that if the addressed tab shifts
// mid-connection, later input/resize keep hitting the old index — the positional
// misroute the stable id exists to prevent, reintroduced on this path (#1779). This
// The resolve and remote ordinal lookup are not one atomic step, but this site is
// exempt from #2200's shifting-roster race: remote workspaces advertise
// TabManagement=false and the headless agent-server's tab roster is fixed for its
// lifetime, so there is no close/reorder writer on either side. If remote tabs ever
// become mutable, their wire protocol must gain an id-native plane before that
// capability is enabled. A tab that no longer resolves is ErrTabGone — the op is
// refused, not re-pointed.
type resolvingTabBinding struct {
	as       session.AgentServer
	instance *session.Instance
	tabID    string
}

func (b resolvingTabBinding) resolve() (int, error) {
	idx, ok := b.instance.TabIndexByID(b.tabID)
	if !ok {
		return 0, fmt.Errorf("session %q tab id %q: %w", b.instance.Title, b.tabID, session.ErrTabGone)
	}
	return idx, nil
}

func (b resolvingTabBinding) subscribe(since session.Seq) (session.PTYSubscription, error) {
	idx, err := b.resolve()
	if err != nil {
		return nil, err
	}
	return b.as.Subscribe(idx, since)
}

func (b resolvingTabBinding) input(p []byte) error {
	idx, err := b.resolve()
	if err != nil {
		return err
	}
	return b.as.Input(idx, p)
}

func (b resolvingTabBinding) resize(rows, cols uint16) error {
	idx, err := b.resolve()
	if err != nil {
		return err
	}
	return b.as.Resize(idx, rows, cols)
}

// bindTab chooses how this connection addresses its tab. A ?tab_id= binds the
// id-native plane when the runtime has one (the local agent-server), which is the
// race-free path; a runtime without one re-resolves the id per operation. Either
// way an id-addressed connection NEVER pins an ordinal. No ?tab_id= at all is a
// legacy client: pin the ordinal it asked for, exactly as before #1738.
func (cs *controlServer) bindTab(as session.AgentServer, instance *session.Instance, tabID string, tab int) (ptyTabBinding, error) {
	if tabID == "" {
		return ordinalTabBinding{as: as, tab: tab}, nil
	}
	if ta, ok := as.(session.TabAddressableServer); ok {
		return idTabBinding{as: ta, tabID: tabID}, nil
	}
	return resolvingTabBinding{as: as, instance: instance, tabID: tabID}, nil
}

// httpStatusForTab maps a tab-addressing failure to its status: a stale/unknown
// stable id is a 404 (the tab is GONE — the client should stop addressing it), any
// other bind/subscribe failure stays a 500.
func httpStatusForTab(err error) int {
	if errors.Is(err, session.ErrTabGone) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// servePTYStream runs the three loops of one subscriber's connection until any of
// them ends: the writer (ring → PTY_OUT / resize-echo), the reader (INPUT/RESIZE/
// detach → agent-server), and the keepalive pinger. It owns closing the
// subscription and the socket.
func servePTYStream(binding ptyTabBinding, sub session.PTYSubscription, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = sub.Close() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); readPTYClient(ctx, binding, conn) }()
	go func() { defer wg.Done(); defer cancel(); keepalivePTY(ctx, conn) }()

	writePTYStream(ctx, sub, conn)

	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "")
	wg.Wait()
}

// writePTYStream is the single writer goroutine: it multiplexes the subscriber's
// output bytes and resize echoes onto the one connection. Keeping it single means
// no concurrent data writes race on the socket.
func writePTYStream(ctx context.Context, sub session.PTYSubscription, conn *websocket.Conn) {
	// Emit the start-seq hello as the VERY FIRST frame, before any PTY_OUT/repaint,
	// so a browser client — which cannot read the X-Af-Stream-Seq handshake header
	// off a WS upgrade (#1592 Phase 5 PR1, §4.3) — learns its absolute cursor in-band
	// to seed ?since replay. sub.Seq() here is still the subscription's start cursor
	// (no PTYData consumed yet, and repaint/resize don't advance it), so it is
	// byte-identical to the streamSeqHeader value set at handshake. Additive: Go
	// stream consumers (TUI/apiclient) decode the frame cleanly and skip it — no
	// behavior change (see agentproto OpHello).
	hctx, hcancel := context.WithTimeout(ctx, wsWriteTimeout)
	err := agentproto.WriteFrame(hctx, conn, agentproto.HelloFrame(uint64(sub.Seq())))
	hcancel()
	if err != nil {
		return // wedged/dead client before the first byte: drop it (session untouched)
	}
	for {
		ev, err := sub.NextEvent(ctx)
		if err != nil {
			// Distinguish a SESSION-END (the broker closed because the session was
			// killed/archived or the agent-server torn down → NextEvent returns
			// io.EOF) from THIS subscriber going away (the reader/keepalive cancelled
			// ctx on a client drop → ctx.Err() != nil). On a session-end, tell the
			// client explicitly with a MsgExit control frame so a browser terminal
			// settles to an "exited" state and STOPS reconnecting, instead of
			// reconnect-looping against a session that no longer exists (#1592 Phase
			// 5 PR5). A ctx cancellation is a normal client-side teardown — nothing to
			// announce; the socket close alone is the signal, and the client (if still
			// alive) reconnects. This brings the local broker in line with the remote
			// agent-server, which already emits MsgExit that the TUI attach consumes
			// (apiclient/attach.go). Go stream consumers (TUI live panes) ignore an
			// unrecognized control frame, so the added frame is safe for them.
			//
			// A tab close (#2136) is a third case that used to be NEITHER: CloseTab
			// killed the tab's tmux without touching its broker, so NextEvent never
			// returned and a PTY-only client sat on a dead stream until the keepalive
			// dropped it ~15s later. It now ends the stream with ErrTabClosed, which
			// WRAPS io.EOF — so it takes the same "tell the client" branch, and the only
			// difference on the wire is the exit's additive reason field. An old client
			// reads a plain exit and settles exactly as it does on a session end (which
			// is right: this stream is over either way); a client that reads the reason
			// can say "tab closed" and leave the session's other tabs alone.
			if ctx.Err() == nil && errors.Is(err, io.EOF) {
				exit := agentproto.NewExitMessage(0)
				if errors.Is(err, session.ErrTabClosed) {
					exit = agentproto.NewTabClosedMessage()
				}
				ectx, ecancel := context.WithTimeout(context.Background(), wsWriteTimeout)
				_ = agentproto.WriteControl(ectx, conn, exit)
				ecancel()
			}
			return
		}
		wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
		switch ev.Kind {
		case session.PTYData:
			err = agentproto.WriteFrame(wctx, conn, agentproto.PTYOutFrame(ev.Data))
		case session.PTYRepaint:
			err = agentproto.WriteFrame(wctx, conn, agentproto.RepaintFrame(ev.Data))
		case session.PTYCursor:
			// The broker fast-forwarded this subscriber over bytes that no longer exist
			// (a ring eviction or the #1840 recovery discard). Re-seed the client's
			// cursor with the same OpHello frame the subscription opened with, so its
			// next ?since matches the server's true position instead of the stale
			// start + bytes-received arithmetic (which would replay already-rendered
			// bytes on reconnect — the #1845 follow-up).
			err = agentproto.WriteFrame(wctx, conn, agentproto.HelloFrame(uint64(ev.Seq)))
		case session.PTYResize:
			err = agentproto.WriteControl(wctx, conn, agentproto.NewResizeMessage(ev.Rows, ev.Cols))
		}
		wcancel()
		if err != nil {
			return // wedged/dead client: drop it (the session is untouched)
		}
	}
}

// readPTYClient handles client → server frames: INPUT and RESIZE are applied to
// the agent-server (multi-writer, from any subscriber); a detach control frame or
// any read error ends the connection.
func readPTYClient(ctx context.Context, binding ptyTabBinding, conn *websocket.Conn) {
	for {
		msg, err := agentproto.ReadMessage(ctx, conn)
		if err != nil {
			return
		}
		if msg.Binary {
			// The binding re-resolves the tab per frame (#1738): for a ?tab_id=
			// connection each keystroke/resize lands on wherever THAT tab now sits, so a
			// mid-connection reorder/close can't misroute it. A tab that has since been
			// closed no longer resolves — the op errors (ErrTabGone) and the frame is
			// dropped rather than routed to whatever tab now holds the old ordinal.
			switch msg.Frame.Op {
			case agentproto.OpInput:
				_ = binding.input(msg.Frame.Data)
			case agentproto.OpResize:
				_ = binding.resize(msg.Frame.Rows, msg.Frame.Cols)
			}
			continue
		}
		if t, _ := agentproto.MessageTypeOf(msg.Text); t == agentproto.MsgDetach {
			return
		}
	}
}

// keepalivePTY pings the subscriber every wsKeepaliveInterval; a missed pong
// within the same interval drops this (dead) subscriber. Dropping never touches
// the PTY/session or other subscribers.
func keepalivePTY(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(wsKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, pcancel := context.WithTimeout(ctx, wsKeepaliveInterval)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// streamInfoResponse tells a client WHERE a session's PTY stream is reachable —
// the "expose an authed URL" indirection (§4.2). For the local runtime it is the
// relative stream path on this same socket; a Phase-4 remote/container runtime
// returns its own authed http:// URL, and the client dials whatever it is handed
// without knowing which runtime backs the session.
type streamInfoResponse struct {
	URL   string `json:"url"`
	Local bool   `json:"local"`
}

// streamInfoHandler answers GET /v1/sessions/{id}/stream-info with the stream
// endpoint for the session. It resolves the session (404 if absent) and reads its
// agent-server's Expose() handle.
func (cs *controlServer) streamInfoHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	// Same warm-up gate as streamHandler (#2109), and needed for the same reason:
	// this route resolves the session through the very code path that builds
	// instances off disk, so an ungated call during the restore window orphans what
	// it just resolved. Answering the stream's ADDRESS is not a lesser read — it is
	// the call a client makes immediately before dialing the stream itself.
	if err := cs.requireManagerReady(); err != nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, err)
		return
	}
	id := r.PathValue("id")
	repoID := r.URL.Query().Get("repo_id")
	as, _, err := cs.manager.agentServerForStream(id, repoID)
	if err != nil {
		writeHTTPError(w, r, http.StatusNotFound, err)
		return
	}
	ep, err := as.Expose()
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
		return
	}
	resp := streamInfoResponse{Local: ep.Local}
	if ep.URL != "" {
		resp.URL = ep.URL // a remote/container runtime's own authed URL (Phase 4)
	} else {
		resp.URL = localStreamPath(id, repoID)
	}
	writeHTTPSuccess(w, r, resp)
}

// localStreamPath builds the relative stream URL for a local session, escaping
// the id and carrying repo_id through so the client dials the same session the
// info request named.
func localStreamPath(id, repoID string) string {
	p := "/v1/sessions/" + url.PathEscape(id) + "/stream"
	if repoID != "" {
		p += "?repo_id=" + url.QueryEscape(repoID)
	}
	return p
}

// parseSince parses the ?since=<seq> replay cursor. Empty means 0 (live tail).
func parseSince(raw string) (session.Seq, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid since cursor %q: %w", raw, err)
	}
	return session.Seq(v), nil
}

// parseTab parses the ?tab=<idx> tab selector (#1592 Phase 2 PR6). Empty means 0
// (the agent tab); a non-negative integer selects a shell/process tab.
func parseTab(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid tab index %q", raw)
	}
	return v, nil
}
