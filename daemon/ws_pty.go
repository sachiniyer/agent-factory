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
		writeHTTPError(w, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	id := r.PathValue("id")
	repoID := r.URL.Query().Get("repo_id")
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	tab, err := parseTab(r.URL.Query().Get("tab"))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, err)
		return
	}
	as, err := cs.manager.agentServerForStream(id, repoID)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound, err)
		return
	}
	sub, err := as.Subscribe(tab, since)
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, err)
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
	servePTYStream(as, tab, sub, conn)
}

// servePTYStream runs the three loops of one subscriber's connection until any of
// them ends: the writer (ring → PTY_OUT / resize-echo), the reader (INPUT/RESIZE/
// detach → agent-server), and the keepalive pinger. It owns closing the
// subscription and the socket.
func servePTYStream(as session.AgentServer, tab int, sub session.PTYSubscription, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = sub.Close() }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); readPTYClient(ctx, as, tab, conn) }()
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
			if ctx.Err() == nil && errors.Is(err, io.EOF) {
				ectx, ecancel := context.WithTimeout(context.Background(), wsWriteTimeout)
				_ = agentproto.WriteControl(ectx, conn, agentproto.NewExitMessage(0))
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
func readPTYClient(ctx context.Context, as session.AgentServer, tab int, conn *websocket.Conn) {
	for {
		msg, err := agentproto.ReadMessage(ctx, conn)
		if err != nil {
			return
		}
		if msg.Binary {
			switch msg.Frame.Op {
			case agentproto.OpInput:
				_ = as.Input(tab, msg.Frame.Data)
			case agentproto.OpResize:
				_ = as.Resize(tab, msg.Frame.Rows, msg.Frame.Cols)
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
		writeHTTPError(w, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	id := r.PathValue("id")
	repoID := r.URL.Query().Get("repo_id")
	as, err := cs.manager.agentServerForStream(id, repoID)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound, err)
		return
	}
	ep, err := as.Expose()
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, err)
		return
	}
	resp := streamInfoResponse{Local: ep.Local}
	if ep.URL != "" {
		resp.URL = ep.URL // a remote/container runtime's own authed URL (Phase 4)
	} else {
		resp.URL = localStreamPath(id, repoID)
	}
	writeHTTPSuccess(w, resp)
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
