package apiclient

import (
	"context"
	"fmt"
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

// DialStream opens a WS subscription to tab `tab` of the session (title, optional
// repoID) starting at output cursor `since` (0 = the live tail). The read/write
// framing is agentproto's; this only establishes the connection and reports the
// server's starting cursor. The caller owns Conn and must Close it.
func (c *Client) DialStream(ctx context.Context, title, repoID string, tab int, since uint64) (*StreamConn, error) {
	q := url.Values{}
	if repoID != "" {
		q.Set("repo_id", repoID)
	}
	if tab != 0 {
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
	u := c.wsBase + "/v1/sessions/" + url.PathEscape(title) + "/stream"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// A REMOTE target bounds the UPGRADE handshake so a peer that accepts the TCP
	// connection but never answers the 101 can't hang the attach path (which dials
	// with context.Background()) — plain HTTP has no TLS handshake timeout to lean
	// on. The deadline governs ONLY the handshake: coder/websocket's Dial does not
	// use the context for the established stream, so cancelling it after Dial
	// returns never severs the live subscription. The local unix socket keeps
	// context.Background() (the socket is local — there or not, bounded by dial).
	dialCtx := ctx
	if c.requestTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, remoteWSHandshakeTimeout)
		defer cancel()
	}
	conn, resp, err := websocket.Dial(dialCtx, u, &opts)
	if err != nil {
		return nil, &TransportError{Err: fmt.Errorf("apiclient: dial pty stream: %w", err)}
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

// Preview captures a session tab's content through the daemon — the sole capturer
// after PR6. It returns the captured content, or gone=true when the session's tmux
// vanished mid-capture (the caller maps that to its session-gone fallback). Used
// for surfaces the TUI can't stream live: remote/hook sessions, scroll-mode
// scrollback (full=true), and the transient preview target.
func (c *Client) Preview(req daemon.PreviewRequest) (content string, gone bool, err error) {
	var resp daemon.PreviewResponse
	if err := c.call("Preview", req, &resp); err != nil {
		return "", false, err
	}
	return resp.Content, resp.Gone, nil
}
