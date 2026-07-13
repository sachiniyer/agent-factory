package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// fakeHeadlessAgentServer is an in-memory session.AgentServer for the headless
// wiring test: it records the control calls, returns canned observations, and
// backs Subscribe/Input with a simple channel fan-out so the WS PTY path can be
// exercised end to end without tmux. It lets the headless HTTP/WS surface be
// proven fast and anywhere (no real session, no daemon spawn).
type fakeHeadlessAgentServer struct {
	mu           sync.Mutex
	provisioned  bool
	launched     bool
	killed       bool
	lastPrompt   string
	tapped       int
	subs         map[*fakeSub]struct{}
	snapshotText string
}

func newFakeHeadlessAgentServer() *fakeHeadlessAgentServer {
	return &fakeHeadlessAgentServer{subs: map[*fakeSub]struct{}{}, snapshotText: "❯ ready"}
}

var _ session.AgentServer = (*fakeHeadlessAgentServer)(nil)

func (f *fakeHeadlessAgentServer) Provision(bool) error { f.set(&f.provisioned); return nil }
func (f *fakeHeadlessAgentServer) Launch(bool) error    { f.set(&f.launched); return nil }
func (f *fakeHeadlessAgentServer) Expose() (session.StreamEndpoint, error) {
	return session.StreamEndpoint{Local: true}, nil
}
func (f *fakeHeadlessAgentServer) Snapshot() (session.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return session.Observation{Updated: true, HasPrompt: false, Content: f.snapshotText}, nil
}
func (f *fakeHeadlessAgentServer) Preview(int, bool) (string, error) { return "preview-body", nil }
func (f *fakeHeadlessAgentServer) Alive() bool                       { return true }
func (f *fakeHeadlessAgentServer) Archive() (string, error)          { return "session/fake", nil }
func (f *fakeHeadlessAgentServer) SendPrompt(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPrompt = p
	return nil
}
func (f *fakeHeadlessAgentServer) TapEnter() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tapped++
}

func (f *fakeHeadlessAgentServer) Subscribe(_ int, _ session.Seq) (session.PTYSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSub{ch: make(chan session.PTYEvent, 16)}
	f.subs[s] = struct{}{}
	return s, nil
}

func (f *fakeHeadlessAgentServer) Input(_ int, b []byte) error {
	// Echo the input to every subscriber as PTY output, so a WS client that types
	// sees its bytes come back over the stream (proving the round trip).
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		select {
		case s.ch <- session.PTYEvent{Kind: session.PTYData, Data: append([]byte("echo:"), b...)}:
		default:
		}
	}
	return nil
}

func (f *fakeHeadlessAgentServer) Resize(_ int, _, _ uint16) error { return nil }

func (f *fakeHeadlessAgentServer) Kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	for s := range f.subs {
		close(s.ch)
		delete(f.subs, s)
	}
	return nil
}

func (f *fakeHeadlessAgentServer) set(b *bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*b = true
}

// fakeSub is a channel-backed PTYSubscription for the fake agent-server.
type fakeSub struct {
	ch chan session.PTYEvent
}

func (s *fakeSub) NextEvent(ctx context.Context) (session.PTYEvent, error) {
	select {
	case <-ctx.Done():
		return session.PTYEvent{}, ctx.Err()
	case ev, ok := <-s.ch:
		if !ok {
			return session.PTYEvent{}, io.EOF
		}
		return ev, nil
	}
}
func (s *fakeSub) Seq() session.Seq { return 0 }
func (s *fakeSub) Close() error     { return nil }

// TestHeadlessAgentServer_TLSTokenRoundTrip drives the headless agent-server's
// wiring in-process over a REAL loopback TLS+token listener: the control REST
// mirror of session.AgentServer and the WS PTY stream, both behind the bearer
// token, plus a missing-token rejection. It proves the routes are wired to the
// single AgentServer and gated exactly like the daemon surface — without a real
// session or a spawned daemon.
func TestHeadlessAgentServer_TLSTokenRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	fake := newFakeHeadlessAgentServer()
	hs := &headlessServer{as: fake, title: "probe", events: newEventsHub()}

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	// The agent-server uses the strict zero-value policy: token mandatory for
	// every peer, loopback NOT exempt — so "missing token → 401" holds even over
	// this loopback socket (#1696).
	closeTCP, info, err := startTCPListener(hs.newMux(), cfg, tokenGatePolicy{})
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()
	require.NotEmpty(t, info.Token)

	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: pinnedTLSConfig(t, dir+"/"+daemonTLSCertFileName)},
	}
	baseURL := "https://" + info.Addr

	post := func(path, body string) (*http.Response, error) {
		req, rerr := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
		require.NoError(t, rerr)
		req.Header.Set("Authorization", "Bearer "+info.Token)
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}
	// decodeData decodes the {data,error} envelope's data member into dst.
	decodeData := func(resp *http.Response, dst any) {
		var env struct {
			Data  json.RawMessage `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
		_ = resp.Body.Close()
		require.Nil(t, env.Error)
		if dst != nil {
			require.NoError(t, json.Unmarshal(env.Data, dst))
		}
	}

	// --- control REST: provision + launch (create the session) --------------
	resp, err := post("/v1/agent/provision", `{"first_time_setup":true}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	decodeData(resp, nil)
	resp, err = post("/v1/agent/launch", `{"first_time_setup":true}`)
	require.NoError(t, err)
	decodeData(resp, nil)
	require.True(t, fake.provisioned)
	require.True(t, fake.launched)

	// --- control REST: snapshot ---------------------------------------------
	resp, err = post("/v1/agent/snapshot", ``)
	require.NoError(t, err)
	var snap agentSnapshotResponse
	decodeData(resp, &snap)
	require.Equal(t, "❯ ready", snap.Content)
	require.True(t, snap.Updated)

	// --- control REST: alive / send-prompt / tap-enter ----------------------
	resp, err = post("/v1/agent/alive", ``)
	require.NoError(t, err)
	var alive agentAliveResponse
	decodeData(resp, &alive)
	require.True(t, alive.Alive)

	resp, err = post("/v1/agent/send-prompt", `{"prompt":"hello world"}`)
	require.NoError(t, err)
	decodeData(resp, nil)
	require.Equal(t, "hello world", fake.lastPrompt)

	// --- control REST: archive returns the pushed branch (#1592 Phase 4 PR6) --
	resp, err = post("/v1/agent/archive", ``)
	require.NoError(t, err)
	var archived agentArchiveResponse
	decodeData(resp, &archived)
	require.Equal(t, "session/fake", archived.Branch)

	// --- REST: missing token → 401 ------------------------------------------
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/agent/snapshot", nil)
	require.NoError(t, err)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	// --- WS PTY stream: subscribe, type input, see it echoed back -----------
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx,
		"wss://"+info.Addr+"/v1/sessions/probe/stream?"+agentproto.AccessTokenQueryParam+"="+info.Token,
		&websocket.DialOptions{HTTPClient: client})
	require.NoError(t, err, "authorized WS handshake must upgrade")
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// The very first frame on a fresh subscription is the OpHello start-seq (#1592
	// Phase 5 PR1): the in-band cursor seed a browser needs because it cannot read the
	// X-Af-Stream-Seq handshake header off the WS upgrade. Assert it here on the real
	// TLS/WSS transport this test drives.
	first, err := agentproto.ReadMessage(ctx, conn)
	require.NoError(t, err)
	require.True(t, first.Binary)
	require.Equal(t, agentproto.OpHello, first.Frame.Op, "first stream frame must be the start-seq hello")

	// Then type input and assert it echoes back as PTY output, skipping any initial
	// repaint/resize frames exactly as a real stream consumer (app/live_stream
	// apiStream) does — it collects OpPTYOut and ignores everything else.
	require.NoError(t, agentproto.WriteFrame(ctx, conn, agentproto.InputFrame([]byte("typed\n"))))
	var echoed []byte
	for !strings.Contains(string(echoed), "echo:typed\n") {
		msg, rerr := agentproto.ReadMessage(ctx, conn)
		require.NoError(t, rerr) // ctx deadline bounds the loop if the echo never lands
		if msg.Binary && msg.Frame.Op == agentproto.OpPTYOut {
			echoed = append(echoed, msg.Frame.Data...)
		}
	}

	// --- WS PTY stream: no token → handshake rejected -----------------------
	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	badConn, _, err := websocket.Dial(badCtx, "wss://"+info.Addr+"/v1/sessions/probe/stream",
		&websocket.DialOptions{HTTPClient: client})
	if badConn != nil {
		_ = badConn.Close(websocket.StatusNormalClosure, "")
	}
	require.Error(t, err, "unauthenticated WS handshake must be rejected")
}
