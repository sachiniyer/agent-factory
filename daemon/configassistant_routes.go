package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/session"
)

// The web config-assistant HTTP surface (#2467). Three routes, all behind the same
// withAuth gate every other route rides (httpauth.go) — no per-route credential:
//
//	POST   /v1/config-assistant         spawn-or-reuse the shared assistant (blocks ~60s cold)
//	GET    /v1/config-assistant/stream  the live PTY WebSocket (browser: ?access_token=)
//	DELETE /v1/config-assistant         reap it now
//
// The path names NO session. A generic "stream a tmux session by name" route,
// combined with the daemon's flat token model (any token-holder, no per-session
// ACL — httpauth.go), would let any authenticated client stream any af_* tmux
// session on the box; scoping the route to the daemon-owned assistant makes that
// structurally impossible rather than merely denied. The daemon resolves THE
// assistant internally (configAssistantHub), so the browser addresses nothing and
// supplies nothing that becomes a command — the spawn request is built entirely
// server-side from the daemon's own config.
//
// Token handling follows the existing PTY stream: a browser cannot set an
// Authorization header on a WS handshake, so the token rides ?access_token= exactly
// as /v1/sessions/{id}/stream does. Neither handler logs the request URL, so the
// token never reaches a log (#2461). If a future error path must log a URL here, it
// MUST scrub it with redactAccessTokenInURL — url.Redacted() leaves a query token
// intact (see session/agentserver_remote.go). Known, accepted limitation (per the
// #2467 decision): the durable token still rides every stream URL repo-wide, which
// is fine behind the tailnet-only listener; a repo-wide move to WS tickets is not
// in scope.

// configAssistantResponse is the POST/DELETE JSON body. SessionName is
// informational (logs/debugging) — the stream route resolves the assistant itself
// and accepts no name, so a client never needs to echo it back.
type configAssistantResponse struct {
	SessionName string `json:"session_name,omitempty"`
}

// configAssistantHandler spawns-or-reuses the shared assistant. It can block for
// the cold-start readiness + trust-dismissal + briefing wait (~60s), which is why
// it is a plain POST and not folded into the WS upgrade: a browser must not see a
// 60s-hung handshake. r.Context() cancellation (client hangup) aborts the spawn.
func (cs *controlServer) configAssistantHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	if err := cs.requireStateMutationAdmission(); err != nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, err)
		return
	}
	name, err := cs.manager.configAssistants.ensure(r.Context())
	if err != nil {
		writeHTTPError(w, r, statusForConfigAssistant(err), err)
		return
	}
	writeHTTPSuccess(w, r, configAssistantResponse{SessionName: name})
}

// configAssistantStreamHandler upgrades to a WebSocket and serves the shared
// assistant's PTY. It resolves the assistant BEFORE the upgrade so a missing one
// returns an HTTP 404 (the browser terminal settles on MsgExit and stops
// reconnecting) rather than a post-upgrade close the client reads as a transient
// drop.
func (cs *controlServer) configAssistantStreamHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	if err := cs.requireStateMutationAdmission(); err != nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, err)
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	sub, streamer, err := cs.manager.configAssistants.subscribe(since)
	if err != nil {
		writeHTTPError(w, r, statusForConfigAssistant(err), err)
		return
	}
	// Announce the start cursor on the handshake response, as the session stream
	// does, so a Go client can seed ?since; the browser reads the same value from
	// the in-band OpHello frame servePTYStream emits first.
	w.Header().Set(streamSeqHeader, strconv.FormatUint(uint64(sub.Seq()), 10))
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		_ = sub.Close()
		return
	}
	servePTYStream(bareSessionBinding{streamer: streamer}, sub, conn)
}

// configAssistantDeleteHandler reaps the shared assistant now — the web analog of
// the TUI's handleConfigAgentDone. Idempotent: reaping when none runs succeeds.
func (cs *controlServer) configAssistantDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	if err := cs.manager.configAssistants.reap(); err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, err)
		return
	}
	writeHTTPSuccess(w, r, configAssistantResponse{})
}

// statusForConfigAssistant maps a hub error to its HTTP status:
//   - errNoConfigAssistant → 404, the STREAM route's settle-and-stop signal (the
//     assistant is gone; the browser terminal stops reconnecting). Only the stream
//     route ever produces it.
//   - errConfigAssistantSpawnAborted → 409, a POST whose create raced a concurrent
//     DELETE. RETRYABLE — a second POST would succeed — so it must NOT be the 404 a
//     client reads as "give up" (#2467 review round 2).
//   - errConfigAssistantUnavailable → 503, the feature is absent from this build.
//   - anything else (a genuine spawn failure) → 500.
func statusForConfigAssistant(err error) int {
	switch {
	case errors.Is(err, errNoConfigAssistant):
		return http.StatusNotFound
	case errors.Is(err, errConfigAssistantSpawnAborted):
		return http.StatusConflict
	case errors.Is(err, errConfigAssistantUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// bareSessionBinding addresses the shared assistant's single pane for one WS
// connection's life. It is the ptyTabBinding for a bare session: there is no tab to
// resolve, so input/resize go straight to the streamer the subscription came from —
// pinned to THAT streamer, not the hub's mutable current, so a reap+respawn mid-
// connection can never misroute a keystroke into a different assistant.
type bareSessionBinding struct {
	streamer bareStreamer
}

func (b bareSessionBinding) subscribe(since session.Seq) (session.PTYSubscription, error) {
	return b.streamer.Subscribe(since)
}
func (b bareSessionBinding) input(p []byte) error           { return b.streamer.Input(p) }
func (b bareSessionBinding) resize(rows, cols uint16) error { return b.streamer.Resize(rows, cols) }
