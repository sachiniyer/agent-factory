package apiclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The client side of the WS PTY data plane (#1592 Phase 2 PR6). The daemon has
// served GET /v1/sessions/{id}/stream since PR5; this is the first consumer. It
// dials the same daemon-http.sock the read/control client uses — the WS handshake
// rides the very http.Client whose transport dials the Unix socket — so a live
// pane streams over the socket with no extra transport, port, or auth wiring. The
// ui/termpane emulator consumes the raw PTY_OUT bytes exactly as it consumed tmux
// attach output before PR6.

// StreamConn is one open WS subscription to a session tab's PTY stream. The caller
// reads PTY_OUT / resize-echo frames and writes INPUT / RESIZE frames through the
// embedded *websocket.Conn using agentproto's codec; StartSeq is the absolute
// cursor the server will begin sending from, so a reconnect resumes with
// ?since=<StartSeq + bytesReceived> to replay the gap.
type StreamConn struct {
	Conn     *websocket.Conn
	startSeq uint64
}

// StartSeq is the absolute output cursor this subscription starts at (the
// X-Af-Stream-Seq handshake header). A client tracks its position as
// StartSeq + bytesReceived and reconnects with ?since=<that> to replay a drop.
func (s *StreamConn) StartSeq() uint64 { return s.startSeq }

// StreamSession names the session a WS stream dials. ID, when non-empty, is
// authoritative: the dial is sent as by=id with no repo scope, so the daemon
// resolves the stable id and REFUSES a miss rather than falling back to a title
// — a deleted session's id cannot reach a different session whose title spells
// the same bytes (#4760 review). With no ID the legacy shape applies: Title in
// the path, scoped by RepoID when set.
type StreamSession struct {
	ID     string
	Title  string
	RepoID string
}

// DialStream opens a WS subscription to a tab of the session s starting at
// output cursor `since` (0 = the live tail). The tab is
// addressed by its stable id (#1738) when tabID is non-empty — so a reorder/close
// can't misroute the stream — and by the ordinal `tab` otherwise (legacy
// positional path). The read/write framing is agentproto's; this only establishes
// the connection and reports the server's starting cursor. The caller owns Conn
// and must Close it.
func (c *Client) DialStream(ctx context.Context, s StreamSession, tabID string, tab int, since uint64) (*StreamConn, error) {
	q := url.Values{}
	segment := s.Title
	if s.ID != "" {
		segment = s.ID
		q.Set(agentproto.StreamAddressQueryParam, agentproto.StreamAddressByID)
	} else if s.RepoID != "" {
		q.Set("repo_id", s.RepoID)
	}
	if tabID != "" {
		q.Set("tab_id", tabID)
	} else if tab != 0 {
		q.Set("tab", strconv.Itoa(tab))
	}
	if since != 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	// For a REMOTE target the token also rides the query string (?access_token=),
	// not just the Authorization header: browsers can't set headers on a WS
	// handshake, so the daemon's extractor honors the query fallback and we use it
	// here too for parity (§1.6). No-op for the local socket (empty token).
	var opts websocket.DialOptions
	opts.HTTPClient = c.httpClient
	if c.token != "" {
		q.Set(agentproto.AccessTokenQueryParam, c.token)
		opts.HTTPHeader = http.Header{}
		c.setAuth(opts.HTTPHeader)
	}
	// wsBase is the placeholder ws://unix for the local socket (the http.Client's
	// transport dials the socket regardless of host) or the real ws://host:port
	// for a remote daemon; either way "ws://" selects the WebSocket handshake.
	u := c.wsBase + "/v1/sessions/" + url.PathEscape(segment) + "/stream"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// A REMOTE target bounds the UPGRADE response separately from the TCP connect.
	// ResponseHeaderTimeout starts only after the request has been written, so a
	// slow-but-valid connect cannot consume the 101 exchange's budget (#2670). The
	// clone is WS-only: putting this bound on the shared REST transport would sever
	// a slow synchronous CreateSession before provisioning finishes. The local Unix
	// socket keeps its existing transport unchanged.
	dialCtx := ctx
	if c.remoteTransport != nil {
		wsTransport := c.remoteTransport.Clone()
		wsTransport.ResponseHeaderTimeout = remoteWSHandshakeTimeout
		defer wsTransport.CloseIdleConnections()
		wsClient := *c.httpClient
		wsClient.Transport = wsTransport
		opts.HTTPClient = &wsClient
		// ResponseHeaderTimeout starts only after net/http finishes writing the
		// upgrade request. Keep a larger overall backstop for a peer that connects
		// but stops reading: dial + upgrade may each consume their full independent
		// budgets, while request writing can never hang forever.
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, remoteDialTimeout+remoteWSHandshakeTimeout)
		defer cancel()
	}
	conn, resp, err := websocket.Dial(dialCtx, u, &opts)
	if err != nil {
		return nil, &TransportError{Err: dialStreamError(err, resp, c.token)}
	}
	// Raise the read limit off the 32 KiB default: a full-screen repaint of a wide
	// pane, or a ?since replay of the whole ring, is a single large binary frame.
	conn.SetReadLimit(4 << 20)
	var start uint64
	if v := resp.Header.Get(streamSeqHeader); v != "" {
		start, _ = strconv.ParseUint(v, 10, 64)
	}
	return &StreamConn{Conn: conn, startSeq: start}, nil
}

// streamSeqHeader mirrors daemon.streamSeqHeader — the handshake response header
// carrying the subscription's starting cursor. Duplicated as a const rather than
// exported from daemon to keep the client's dependency on the daemon package to
// the request/response structs it already shares.
const streamSeqHeader = "X-Af-Stream-Seq"

// streamDialErrorBodyLimit bounds the pre-upgrade body a malicious or buggy peer
// could stream on a non-101 response. The daemon's failure envelope is tiny; this
// only caps the read, not the message notServedDetail renders from it.
const streamDialErrorBodyLimit = 1 << 20 // 1 MiB

// dialStreamError wraps a failed WS handshake with the daemon's JSON error body
// so the user sees the daemon's refusal instead of an opaque status mismatch.
//
// coder/websocket's dial error carries only the handshake status ("expected
// response status code 101 Switching Protocols but got 500"), and the previous
// handler dropped the accompanying response, so the daemon's actionable message
// never reached the CLI: an attach to an Archived session printed a generic
// transport fault with no hint that the session is archived or that
// `af sessions restore` exists. The stream route refuses a non-live target BEFORE
// websocket.Accept (agentServerForStreamTarget's liveness gate), so the refusal
// is a plain non-101 HTTP response whose body is the envelope writeHTTPError
// wrote; reading it here makes that message the one the user reads.
//
// The access-token redaction runs on the whole composed error: the dial URL
// carries ?access_token= for a remote target, so the raw handshake error can
// embed it, and any echoed body is covered by the same pass. Best-effort: a body
// that is not the daemon's envelope falls back to a snippet (notServedDetail),
// and an empty body adds nothing — the handshake status alone is the answer.
func dialStreamError(dialErr error, resp *http.Response, token string) error {
	if detail := streamDialErrorDetail(resp); detail != "" {
		dialErr = fmt.Errorf("%w: %s", dialErr, detail)
	}
	return fmt.Errorf("apiclient: dial pty stream: %w",
		agentproto.RedactAccessTokenError(dialErr, token))
}

// streamDialErrorDetail reads the failed handshake's response body and returns
// the daemon's envelope message (or a snippet for a non-envelope intermediary
// body), preserving resp.Body's Close contract. It returns "" when there is no
// response to read (a transport-level failure before any HTTP answer) or the
// body is empty, so the caller does not append noise to the handshake status.
func streamDialErrorDetail(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, streamDialErrorBodyLimit))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	return notServedDetail(raw)
}

// PreviewSnapshot is the typed preview response used by terminal-aware clients.
// Keeping modes beside content prevents a preview target from inheriting the
// ownership state of whichever pane happened to render before it.
func (c *Client) PreviewSnapshot(req daemon.PreviewRequest) (daemon.PreviewResponse, error) {
	var resp daemon.PreviewResponse
	if err := c.call("Preview", req, &resp); err != nil {
		return daemon.PreviewResponse{}, err
	}
	return resp, nil
}
