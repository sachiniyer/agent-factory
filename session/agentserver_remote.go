package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
)

// remoteAgentServer is the daemon-side AgentServer for a runtime whose agent runs
// in a SANDBOX behind an authed URL (#1592 Phase 4 PR2): a real `af agent-server`
// (PR1) reachable over HTTP/WS+token, on another host/container. It is the
// mirror image of localAgentServer — where localAgentServer drives an in-process
// tmux session directly, this is an HTTP/WS CLIENT to an out-of-process
// agent-server, satisfying the SAME session.AgentServer interface so the daemon's
// observation/delivery paths above it do not change (§0, §1.1).
//
//   - The control plane (Provision/Launch/Expose/Snapshot/Preview/Alive/
//     SendPrompt/TapEnter/Kill) is REST to /v1/agent/* — the 1:1 mirror the
//     agent-server exposes.
//   - The data plane (Subscribe/Input/Resize) is the WS PTY stream to the
//     agent-server, reached through the SAME ptyBroker fan-out the local runtime
//     uses — with a remote WS clientlessChannel in place of the tmux one. So one
//     WS to the sandbox is fanned to N local subscribers, ?since replay and the
//     multi-writer input path work identically, and none of the broker logic is
//     reimplemented (§ boundaries: reuse Phase 1-3, no protocol reimpl).
//
// Transport is Phase-3's: plain HTTP/WS (no TLS — af terminates none of its own;
// the sandbox is reached over a private hop, a docker-published loopback port or
// an ssh-forwarded one) + a bearer token on every REST call and WS handshake, and
// agentproto for the PTY wire frames. It cannot import apiclient (that package
// sits above daemon, which sits above session — a cycle), so it shares the pieces
// that matter through the agentproto/apiproto leaves the whole stack already
// agrees on: the token header (agentproto.AuthHeader), the WS codec
// (agentproto.ReadMessage/WriteFrame), and the {data,error} envelope (apiproto).
// The thin HTTP/WS plumbing below is the only local piece.
//
// DARK for users in PR2: no docker/ssh runtime provisions a sandbox to point one
// at yet (PR3-PR5). It is proven by an out-of-process integration test that
// starts a real `af agent-server` on loopback and drives it through here.
type remoteAgentServer struct {
	rc *remoteAgentClient
	// teardown reaps the sandbox the agent runs in AFTER the in-sandbox workspace
	// is killed over REST (#1592 Phase 4 PR4): `docker rm -f` the container. nil
	// for the PR2 out-of-process case (an `af agent-server` the test owns — nothing
	// for this client to reap). Run best-effort in Kill so a session kill also
	// removes its container; idempotent (the runtime guards it with a sync.Once).
	teardown func() error

	mu sync.Mutex
	// brokers holds one lazy ptyBroker per tab index, exactly like localAgentServer
	// — each fans a single remote WS to N local subscribers. The channel behind
	// each is a remoteClientlessChannel (the remote WS) rather than a tmux one.
	brokers map[int]*ptyBroker
	// closed latches once Kill has run so a Subscribe/Input/Resize racing the kill
	// cannot lazily resurrect a broker (and dial a fresh WS to a torn-down sandbox
	// that never gets closed) — the local runtime's #1632 guard, same reasoning.
	closed bool
}

var _ AgentServer = (*remoteAgentServer)(nil)

// AgentServerEndpoint is the runtime handle that points an Instance at a remote
// agent-server (#1592 Phase 4 PR2): the authed URL of the `af agent-server` in the
// sandbox plus the auth material to reach it. A nil endpoint ⇒ the local
// in-process runtime (the default, unchanged). Phase 4 PR3 generalizes this into a
// Runtime that PROVISIONS the sandbox and fills these in; PR2 only consumes them.
type AgentServerEndpoint struct {
	// URL is the agent-server's plain-HTTP base URL — `http://host:port` or
	// `ws://host:port` (equivalent; both select the same plaintext transport,
	// only the authority is used). A wss://host:port is rejected — the
	// agent-server is HTTP-only.
	URL string
	// Token is the bearer credential presented on every REST call and WS handshake.
	Token string
}

// NewRemoteAgentServer builds a remoteAgentServer that drives the `af agent-server`
// reachable at ep.URL for a single workspace titled `title`. It validates the URL
// up front (no dial) so a bad endpoint fails at construction rather than on first
// use — which is why Instance.AgentServer() (infallible) can build one from an
// endpoint validated at NewInstance. The integration test constructs one here
// directly against a real out-of-process agent-server.
func NewRemoteAgentServer(ep AgentServerEndpoint, title string) (AgentServer, error) {
	rc, err := newRemoteAgentClient(ep, title)
	if err != nil {
		return nil, err
	}
	return &remoteAgentServer{rc: rc}, nil
}

func (s *remoteAgentServer) Provision(firstTimeSetup bool) error {
	return s.rc.call("/v1/agent/provision", agentLifecycleReq{FirstTimeSetup: firstTimeSetup}, nil)
}

func (s *remoteAgentServer) Launch(firstTimeSetup bool) error {
	return s.rc.call("/v1/agent/launch", agentLifecycleReq{FirstTimeSetup: firstTimeSetup}, nil)
}

func (s *remoteAgentServer) Expose() (StreamEndpoint, error) {
	// The daemon PROXIES a remote session's stream: it subscribes to the sandbox
	// (Subscribe below dials the remote WS) and re-fans to its own subscribers, so
	// from a TUI's perspective the stream is still reachable at the daemon's local
	// path — the daemon relays. Returning Local=true keeps the daemon's stream
	// handler on its proxy path and the TUI unchanged. The stream-info-returns-a-URL
	// indirection (a browser dialing the sandbox directly) is not implemented —
	// browsers proxy through the daemon instead.
	return StreamEndpoint{Local: true}, nil
}

func (s *remoteAgentServer) Snapshot() (Observation, error) {
	var resp agentSnapshotResp
	if err := s.rc.call("/v1/agent/snapshot", struct{}{}, &resp); err != nil {
		return Observation{}, err
	}
	return Observation{Updated: resp.Updated, HasPrompt: resp.HasPrompt, Content: resp.Content}, nil
}

func (s *remoteAgentServer) Preview(tab int, full bool) (string, error) {
	return s.rc.preview(tab, full)
}

func (s *remoteAgentServer) Alive() bool {
	var resp agentAliveResp
	if err := s.rc.call("/v1/agent/alive", struct{}{}, &resp); err != nil {
		return false
	}
	return resp.Alive
}

func (s *remoteAgentServer) SendPrompt(prompt string) error {
	return s.rc.call("/v1/agent/send-prompt", agentSendPromptReq{Prompt: prompt}, nil)
}

func (s *remoteAgentServer) TapEnter() {
	// Mirrors localAgentServer.TapEnter: fire-and-forget, no error surfaced (a
	// no-op unless the workspace has AutoYes; the daemon never consumed a result).
	_ = s.rc.call("/v1/agent/tap-enter", struct{}{}, nil)
}

// --- data plane: one remote WS per tab, fanned by the shared ptyBroker ---

// ensureBroker lazily builds the ptyBroker for tab `tab`, bound to a remote WS
// clientlessChannel. Refuses once Kill has latched closed (the #1632 guard): a
// Subscribe racing teardown must not dial a fresh WS to a sandbox being torn down.
func (s *remoteAgentServer) ensureBroker(tab int) (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("remote session %q is being terminated", s.rc.title)
	}
	if br := s.brokers[tab]; br != nil {
		return br, nil
	}
	if s.brokers == nil {
		s.brokers = make(map[int]*ptyBroker)
	}
	br := newPTYBroker(newRemoteClientlessChannel(s.rc, tab))
	s.brokers[tab] = br
	return br, nil
}

func (s *remoteAgentServer) Subscribe(tab int, since Seq) (PTYSubscription, error) {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

func (s *remoteAgentServer) Input(tab int, b []byte) error {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.input(b)
}

func (s *remoteAgentServer) Resize(tab int, rows, cols uint16) error {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
}

func (s *remoteAgentServer) Kill() error {
	// Tear every tab's data plane down first (closing each remote WS so subscribers
	// see io.EOF), latch closed under the same lock that snapshots the brokers so a
	// racing Subscribe can't resurrect one, then kill the remote workspace over REST.
	s.mu.Lock()
	s.closed = true
	brokers := s.brokers
	s.brokers = nil
	s.mu.Unlock()
	for _, br := range brokers {
		br.close()
	}
	// Kill the in-sandbox workspace over REST, THEN reap the sandbox itself. Both
	// run even if the REST kill fails (the container must not leak because its
	// agent-server was already down) — the errors are joined so a caller sees
	// both. teardown is idempotent, so a Kill retry after a partial failure is safe.
	killErr := s.rc.call("/v1/agent/kill", struct{}{}, nil)
	if s.teardown != nil {
		if terr := s.teardown(); terr != nil {
			return errors.Join(killErr, terr)
		}
	}
	return killErr
}

// Archive pushes the sandbox's session branch to origin over the control REST
// (#1592 Phase 4 PR6): the in-sandbox agent-server owns the worktree, so the
// push happens THERE (it commits any uncommitted work and pushes), and this
// returns the branch name the daemon records so a later restore clones it back.
// No teardown here — the daemon runs Kill (which reaps the sandbox) after the
// branch is durable, so archive is push-then-teardown.
func (s *remoteAgentServer) Archive() (string, error) {
	var resp agentArchiveResp
	if err := s.rc.call("/v1/agent/archive", struct{}{}, &resp); err != nil {
		return "", err
	}
	return resp.Branch, nil
}

// --- remote WS clientlessChannel: the sandbox stream as a broker channel ---

// remoteClientlessChannel is the remote runtime's clientlessChannel: it binds ONE
// tab's ptyBroker to the sandbox's WS PTY stream. Where tmuxClientlessChannel
// drives pipe-pane/send-keys/resize-window on a local tmux pane, this drives the
// same three verbs over a single bidirectional WebSocket to the `af agent-server`:
//
//   - StartCapture dials the tab's WS and returns a reader that yields the
//     sandbox's PTY_OUT bytes (dropping the remote's own repaint/resize-echo control
//     frames — the daemon-side broker re-derives those locally, so feeding them into
//     the ring would double-count them in the seq).
//   - SendRaw/Resize write INPUT/RESIZE frames back over that same socket (the
//     agent-server accepts input from any subscriber — the multi-writer model).
//   - Snapshot fetches the current screen over REST Preview so the broker can paint
//     a fresh local subscriber, exactly as the tmux channel's capture-pane does.
//
// This is what lets remoteAgentServer reuse ptyBroker unchanged: the broker never
// knows whether its bytes come from a local pipe or a remote socket.
type remoteClientlessChannel struct {
	rc  *remoteAgentClient
	tab int

	mu     sync.Mutex
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// writeMu serializes SendRaw/Resize writes: coder/websocket allows one
	// concurrent reader + one writer, but two goroutines writing (an input and a
	// resize) would race the frame boundary.
	writeMu sync.Mutex
}

var _ clientlessChannel = (*remoteClientlessChannel)(nil)

func newRemoteClientlessChannel(rc *remoteAgentClient, tab int) *remoteClientlessChannel {
	return &remoteClientlessChannel{rc: rc, tab: tab}
}

// StartCapture dials the tab's WS stream (from the live tail) and returns a reader
// over the sandbox's PTY_OUT bytes. The reader goroutine also drains the socket's
// control/repaint frames (dropping them) so the connection stays healthy — coder/
// websocket auto-responds to the server's keepalive pings only while a read is in
// flight, so this loop IS the keepalive. It is the broker's first-Subscribe entry
// point, so the returned reader is consumed immediately and never blocks the socket.
func (c *remoteClientlessChannel) StartCapture() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil, fmt.Errorf("remote clientless capture already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := c.rc.dialStream(ctx, c.tab)
	if err != nil {
		cancel()
		return nil, err
	}
	c.conn, c.ctx, c.cancel = conn, ctx, cancel
	pr, pw := io.Pipe()
	go c.readLoop(ctx, conn, pw)
	return pr, nil
}

// readLoop copies the sandbox's PTY_OUT bytes into the broker's pipe until the
// socket errors or capture is stopped. It DROPS the remote's OpRepaint and every
// control frame (MsgResize echo, MsgExit): the daemon-side broker paints its own
// per-subscriber repaint via Snapshot and manages its own resize echo, so passing
// the remote's through would double-count repaint bytes in the ring seq and echo
// resizes twice. This makes the remote socket behave to the broker exactly like
// tmux pipe-pane: a future-only stream of raw output.
func (c *remoteClientlessChannel) readLoop(ctx context.Context, conn *websocket.Conn, pw *io.PipeWriter) {
	defer func() { _ = pw.Close() }()
	for {
		msg, err := agentproto.ReadMessage(ctx, conn)
		if err != nil {
			return
		}
		if msg.Binary && msg.Frame.Op == agentproto.OpPTYOut {
			if _, werr := pw.Write(msg.Frame.Data); werr != nil {
				return
			}
		}
	}
}

// StopCapture cancels the read loop and closes the WS, so the broker's read side
// (the pipe) returns EOF. Idempotent.
func (c *remoteClientlessChannel) StopCapture() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	c.cancel()
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.conn, c.ctx, c.cancel = nil, nil, nil
	return err
}

// SendRaw writes verbatim input bytes to the sandbox PTY as an INPUT frame over
// the capture socket (multi-writer: the agent-server accepts input from any
// subscriber). It requires the capture socket, which the broker always opens on
// the first Subscribe before any input can be routed here.
func (c *remoteClientlessChannel) SendRaw(b []byte) error {
	conn, ctx, err := c.activeConn()
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, remoteWSWriteTimeout)
	defer cancel()
	return agentproto.WriteFrame(wctx, conn, agentproto.InputFrame(b))
}

// Resize sends the winning size to the sandbox PTY as a RESIZE frame. The broker
// tracks last-resize-wins + echoes the authoritative size to local subscribers;
// this just forwards the size to the remote (whose own echo is dropped by readLoop).
func (c *remoteClientlessChannel) Resize(rows, cols uint16) error {
	conn, ctx, err := c.activeConn()
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, remoteWSWriteTimeout)
	defer cancel()
	return agentproto.WriteFrame(wctx, conn, agentproto.ResizeFrame(rows, cols))
}

// Snapshot returns the sandbox's current visible screen over REST Preview — the
// repaint the broker injects so a fresh local subscriber sees the screen before
// the first live byte, mirroring tmux capture-pane. The WS stream carries only
// future output, so without this a just-opened remote pane would render blank.
func (c *remoteClientlessChannel) Snapshot() (PaneSnapshot, error) {
	content, err := c.rc.preview(c.tab, false)
	if err != nil {
		return PaneSnapshot{}, err
	}
	// REST Preview carries only the screen, not the cursor position, so the repaint
	// omits cursor restore for remote panes (HasCursor stays false). It is also
	// -J-joined (the agent-server's Preview uses CapturePaneContent), NOT the grid form
	// buildRepaint's per-row positioning wants (#1688) — so a wrapped logical line
	// re-wraps by the client width here. This is a known screen-only best-effort
	// limitation of the still-dark remote runtime (no cursor cascade to corrupt, since
	// HasCursor is false); productizing it needs the agent-server to expose a grid
	// capture + cursor over REST, mirroring tmuxClientlessChannel.Snapshot.
	return PaneSnapshot{Screen: []byte(content)}, nil
}

// activeConn returns the live capture socket and its context, or an error if
// capture is not running (no subscriber has opened the stream yet).
func (c *remoteClientlessChannel) activeConn() (*websocket.Conn, context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, nil, fmt.Errorf("remote pty stream for tab %d is not open", c.tab)
	}
	return c.conn, c.ctx, nil
}

// --- remoteAgentClient: the thin REST + WS transport to one agent-server ---

// remoteWSWriteTimeout bounds a single INPUT/RESIZE write to the sandbox socket so
// a wedged connection surfaces as an error rather than blocking a broker goroutine.
const remoteWSWriteTimeout = 10 * time.Second

// remoteAgentDialTimeout bounds the TCP connect to the sandbox (a real network
// hop), and remoteAgentCallTimeout bounds a whole control REST round-trip.
const (
	remoteAgentDialTimeout = 10 * time.Second
	remoteAgentCallTimeout = 30 * time.Second
)

// remoteAgentWSHandshakeTimeout bounds the WS UPGRADE handshake on the internal
// daemon→agent-server stream dial — the 101 exchange after the TCP connect. Plain
// HTTP has no TLS handshake to time out, so without this a wedged agent-server
// that accepts the TCP connection but never answers the upgrade would hang the
// daemon's capture goroutine forever (the #1730 half-open class, on the internal
// hop). It bounds ONLY the handshake: coder/websocket's Dial context does not
// govern the established stream, so a long-lived PTY subscription is never
// severed. A var (not const) so a test can shrink it to prove the bound fires.
var remoteAgentWSHandshakeTimeout = 10 * time.Second

// remoteAgentClient is the transport to ONE `af agent-server`: a token-bearing
// plain-HTTP client for the /v1/agent/* control REST and a matching WS dialer for
// the /v1/sessions/{title}/stream data plane. It is the session-package analogue
// of apiclient.Client (which session cannot import — the cycle), sharing the
// pieces that define the contract through the agentproto/apiproto leaves.
type remoteAgentClient struct {
	httpClient *http.Client
	httpBase   string // http://host:port
	wsBase     string // ws://host:port
	token      string
	title      string // the sandbox's single-workspace session title (the stream path id)
}

// newRemoteAgentClient builds the transport for ep, validating the URL up front
// (no dial). It mirrors apiclient.NewRemote: the transport is plain HTTP/WS (no
// TLS — the sandbox is reached over a private hop) and the token rides every
// request.
func newRemoteAgentClient(ep AgentServerEndpoint, title string) (*remoteAgentClient, error) {
	if title == "" {
		return nil, fmt.Errorf("remote agent-server requires a workspace title")
	}
	httpBase, wsBase, err := splitHTTPBaseURL(ep.URL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteAgentDialTimeout}
	return &remoteAgentClient{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
		},
		httpBase: httpBase,
		wsBase:   wsBase,
		token:    ep.Token,
		title:    title,
	}, nil
}

// call POSTs req as JSON to the agent-server control route at `path`, decodes the
// shared {data,error} envelope, and unmarshals the data member into resp (nil resp
// ⇒ success/failure only). It is the client-side twin of the agent-server's
// rpcHandler dispatch — the same envelope the daemon's own httpserver speaks —
// with the bearer token on every call.
func (c *remoteAgentClient) call(path string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("remote agent-server: marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteAgentCallTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpBase+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("remote agent-server: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(agentproto.AuthHeader, agentproto.BearerScheme+c.token)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("remote agent-server: POST %s: %w", path, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("remote agent-server: read %s response: %w", path, err)
	}
	// Decode into a RawMessage-backed envelope so the data member is unmarshaled in
	// a second pass only when there is no error — the same two-pass decode apiclient
	// uses, so a failure body's null data is never interpreted as the typed struct.
	var env struct {
		Data  json.RawMessage         `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("remote agent-server: malformed %s response envelope: %w", path, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s", env.Error.Message)
	}
	if resp != nil {
		if err := json.Unmarshal(env.Data, resp); err != nil {
			return fmt.Errorf("remote agent-server: malformed %s response data: %w", path, err)
		}
	}
	return nil
}

// preview fetches tab `tab`'s content over the control REST — shared by the
// AgentServer.Preview surface and the broker's fresh-subscriber repaint (Snapshot).
func (c *remoteAgentClient) preview(tab int, full bool) (string, error) {
	var resp agentPreviewResp
	if err := c.call("/v1/agent/preview", agentPreviewReq{Tab: tab, Full: full}, &resp); err != nil {
		return "", err
	}
	return resp.Content, nil
}

// dialStream opens the WS PTY subscription to tab `tab` (from the live tail). The
// token rides both the Authorization header and the ?access_token= query — the
// agent-server honors either, and the query mirrors the browser WS path Phase 5
// needs. The read limit is raised off the 32 KiB default: a repaint of a wide pane
// is a single large binary frame.
func (c *remoteAgentClient) dialStream(ctx context.Context, tab int) (*websocket.Conn, error) {
	q := url.Values{}
	if tab != 0 {
		q.Set("tab", strconv.Itoa(tab))
	}
	q.Set(agentproto.AccessTokenQueryParam, c.token)
	u := c.wsBase + "/v1/sessions/" + url.PathEscape(c.title) + "/stream?" + q.Encode()

	opts := &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: http.Header{agentproto.AuthHeader: []string{agentproto.BearerScheme + c.token}},
	}
	// Bound the UPGRADE handshake so a wedged agent-server (TCP accepted, 101 never
	// sent) can't hang this dial forever. coder/websocket's Dial context bounds only
	// the handshake — the established stream reads use the parent ctx (passed into
	// readLoop), so cancelling this after Dial returns never severs the live stream.
	dialCtx, cancel := context.WithTimeout(ctx, remoteAgentWSHandshakeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, u, opts)
	if err != nil {
		return nil, fmt.Errorf("remote agent-server: dial pty stream: %w", err)
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// splitHTTPBaseURL validates an agent-server base URL and derives its REST
// (http://host:port) and WS (ws://host:port) authorities. It accepts the
// plaintext schemes http/ws interchangeably (the agent-server serves REST and WS
// on one HTTP listener) and rejects the TLS schemes wss/https — the agent-server
// is HTTP-only (af terminates no TLS; the sandbox is reached over a private hop).
// Only scheme+authority are used. It mirrors apiclient's parseDaemonURL but with
// agent-server-appropriate error text.
func splitHTTPBaseURL(raw string) (httpBase, wsBase string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid agent-server URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	case "https", "wss":
		return "", "", fmt.Errorf("agent-server URL %q uses a TLS scheme, but the agent-server is HTTP-only; use http:// or ws://", raw)
	default:
		return "", "", fmt.Errorf("agent-server URL %q must be an http:// or ws:// URL", raw)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("agent-server URL %q has no host:port", raw)
	}
	return "http://" + u.Host, "ws://" + u.Host, nil
}

// --- wire structs: the /v1/agent/* request/response shapes (mirror PR1) ---
//
// These match daemon/agentserver_headless.go's agent* types field-for-field (the
// server side). They are duplicated as unexported session types rather than
// imported because daemon imports session (a cycle the other way); the shared
// contract is the JSON tags, which the round-trip test pins.

type agentLifecycleReq struct {
	FirstTimeSetup bool `json:"first_time_setup"`
}

type agentSnapshotResp struct {
	Updated   bool   `json:"updated"`
	HasPrompt bool   `json:"has_prompt"`
	Content   string `json:"content"`
}

type agentPreviewReq struct {
	Tab  int  `json:"tab"`
	Full bool `json:"full"`
}

type agentPreviewResp struct {
	Content string `json:"content"`
}

type agentAliveResp struct {
	Alive bool `json:"alive"`
}

type agentSendPromptReq struct {
	Prompt string `json:"prompt"`
}

type agentArchiveResp struct {
	Branch string `json:"branch"`
}
