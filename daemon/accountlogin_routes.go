package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/session/accountlogin"
)

// The web's half of `af accounts login` (#3385): one WebSocket route serving the
// PTY of the login pane the daemon owns for one account.
//
//	GET /v1/account-login/stream?agent=<agent>&name=<account>   (browser: ?access_token=)
//
// The CLI and the TUI attach with `tmux attach-session`, which a browser cannot
// run; and a login pane has no Instance, so the ordinary PTY route — which
// resolves its byte source through the daemon's instance map — cannot reach it
// either. This is the same answer the config assistant reached for the same
// shape (#2467), reusing the same machinery: session.BareSessionStreamer for the
// data plane, bareSessionBinding for the connection, servePTYStream for the
// protocol.
//
// WHAT THE CLIENT MAY NAME, which is the part worth being careful about. The
// config-assistant route deliberately names nothing, because a generic "stream a
// tmux session by name" route plus the daemon's flat token model (any
// token-holder, no per-session ACL) would let any authenticated client stream any
// af_* session on the box. This route names an ACCOUNT, not a tmux session, and
// the daemon maps it to a pane through the supervisor that spawned it — so the
// only sessions reachable through it are login panes THIS daemon started, and an
// account that has no flow running answers 404 rather than resolving to anything.
// The account namespace is af's own registry; a name outside it resolves to no
// pane at all.
//
// Token handling follows the existing PTY streams: a browser cannot set an
// Authorization header on a WS handshake, so the token rides ?access_token=
// exactly as /v1/sessions/{id}/stream does. This handler never logs the request
// URL, so the token never reaches a log (#2461); anything added here that logs a
// URL MUST scrub it with agentproto.RedactAccessTokenURL, because url.Redacted()
// leaves a query token intact.
//
// The stream carries the agent's login flow — a device code, a verification URL,
// whatever the CLI prints. It never carries a credential af has read, because af
// reads none: the bytes are the pane's, on their way to a terminal, exactly as
// they would be on the daemon host's own screen.

// accountLoginStreamHandler upgrades to a WebSocket and serves one account's
// login pane.
//
// It resolves the pane BEFORE the upgrade so a login that is not running returns
// an HTTP 404 — which the browser terminal reads as settle-and-stop — rather than
// a post-upgrade close it would read as a transient drop and reconnect through.
// A finished login is not a blip to retry; it is the end of the flow.
func (cs *controlServer) accountLoginStreamHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	if err := cs.requireStateMutationAdmission(); err != nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, err)
		return
	}
	agent := r.URL.Query().Get("agent")
	name := r.URL.Query().Get("name")
	if agent == "" || name == "" {
		writeHTTPError(w, r, http.StatusBadRequest,
			fmt.Errorf("account login stream needs both agent and name query parameters"))
		return
	}
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeHTTPError(w, r, http.StatusBadRequest, err)
		return
	}
	streamer, err := cs.manager.accountLogins.Streamer(agent, name)
	if err != nil {
		writeHTTPError(w, r, statusForAccountLoginStream(err), err)
		return
	}
	sub, err := streamer.Subscribe(since)
	if err != nil {
		writeHTTPError(w, r, statusForAccountLoginStream(err), err)
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
	// Pinned to the streamer the subscription came from, not to a lookup repeated
	// per frame, so a reap and a fresh login mid-connection can never misroute a
	// keystroke into a different pane — the same reason the config assistant's
	// binding pins its own.
	servePTYStream(bareSessionBinding{streamer: streamer}, sub, conn)
}

// statusForAccountLoginStream maps a supervisor error to its HTTP status.
//
// ErrNoLoginPane → 404, which is the browser terminal's settle-and-stop signal.
// It covers both "no flow was ever started for this account" and "the flow
// finished", and those are the same answer to the only question the stream asks:
// there is nothing to watch. Anything else is a genuine failure.
func statusForAccountLoginStream(err error) int {
	if errors.Is(err, accountlogin.ErrNoLoginPane) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
