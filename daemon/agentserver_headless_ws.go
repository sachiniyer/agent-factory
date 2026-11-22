package daemon

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/session"
)

// The headless agent-server's WS data plane (#1592 Phase 4 PR1, split out of
// agentserver_headless.go for the file-length lint #1145): the
// single-workspace variants of the daemon's stream/stream-info/events
// handlers, plus the kill-confirmation workspace measurement.

// --- WS data plane: single-workspace variants of the daemon handlers -------

// streamHandler upgrades GET /v1/sessions/{id}/stream to a WebSocket and fans the
// single workspace's PTY stream. It is the controlServer.streamHandler shape with
// the Manager lookup replaced by the one AgentServer this server owns — the id is
// vestigial (there is exactly one workspace) but the path matches the daemon so a
// driver reuses the identical client. Subscribe happens BEFORE the upgrade so a
// bad cursor or an un-launched workspace returns an HTTP error envelope.
func (hs *headlessServer) streamHandler(w http.ResponseWriter, r *http.Request) {
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
	// The same binding selection the daemon's streamHandler makes (#5571 W4.3):
	// a ?tab_id= binds the id-native plane — which the in-sandbox server's local
	// AgentServer always has — and a missing ?tab_id= stays an ordinal for
	// legacy drivers that predate the parameter. This supersedes the "ordinal
	// only" rule this handler used to carry: the sandbox's tab set is no longer
	// fixed now that /v1/agent/tab-create can grow it, so an id-addressed
	// connection must NEVER re-resolve through a shifting ordinal.
	binding, err := bindTab(hs.as, hs.inst, r.URL.Query().Get("tab_id"), tab)
	if err != nil {
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	sub, err := binding.subscribe(since)
	if err != nil {
		writeHTTPError(w, r, httpStatusForTab(err), err)
		return
	}
	w.Header().Set(streamSeqHeader, strconv.FormatUint(uint64(sub.Seq()), 10))
	// The token gate (withAuth) and CORS allow-list already authenticated the
	// handshake; InsecureSkipVerify defers the Origin check to that gate, exactly
	// as the daemon's streamHandler does.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		_ = sub.Close()
		return
	}
	servePTYStream(binding, sub, conn)
}

// streamInfoHandler answers GET /v1/sessions/{id}/stream-info with where the
// stream is reachable. For the local runtime that is the relative stream path on
// this same listener.
func (hs *headlessServer) streamInfoHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ep, err := hs.as.Expose()
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
		return
	}
	resp := streamInfoResponse{Local: ep.Local}
	if ep.URL != "" {
		resp.URL = ep.URL
	} else {
		resp.URL = localStreamPath(authoritativeStreamTarget(id, ""))
	}
	writeHTTPSuccess(w, r, resp)
}

// eventsHandler upgrades GET /v1/events for surface parity with the daemon. The
// hub is quiet on a single-workspace server (no orchestrator mutations), so this
// proves the plane upgrades and stays alive under the token gate rather than
// carrying session lifecycle events.
func (hs *headlessServer) eventsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	serveEvents(hs.events, conn)
}

// WorkspaceFacts measures this server's workspace for the daemon's kill
// confirmation (#5571 W8.2). Read-only: git status/rev-list/for-each-ref/reflog.
func (hs *headlessServer) WorkspaceFacts(_ struct{}, resp *session.WorkspaceFacts) error {
	if hs.inst == nil {
		return fmt.Errorf("this agent-server has no workspace")
	}
	*resp = session.MeasureWorkspaceAt(hs.inst.GetWorktreePath(), hs.inst.Path)
	return nil
}
