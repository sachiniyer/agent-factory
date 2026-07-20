package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
)

// TestTCPListener_HTTP_TokenRoundTrip is the PR3 payoff, now HTTP-only: a real
// plain-HTTP TCP listener on loopback that REQUIRES the bearer token. It proves
// REST + WS both accept a correct token over http/ws and reject a missing/wrong
// one, with no TLS anywhere (the client is a bare http.Client).
func TestTCPListener_HTTP_TokenRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0" // :0 ⇒ the kernel picks a free port
	m, err := NewManager(cfg)
	require.NoError(t, err)

	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}
	// Strict policy: token mandatory for every peer, loopback NOT exempt. This is
	// the agent-server posture; it lets a real loopback socket still exercise the
	// token-enforcement path (the daemon's loopback-exempt policy is covered by
	// TestTCPListener_LoopbackExempt and the handler-level matrix in httpauth_test).
	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, tokenGatePolicy{}, withWebShell)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()

	require.NotEmpty(t, info.Token)

	// No TLS: prove the daemon serves without any cert on disk (the old self-signed
	// material is gone). daemon-tls.crt must never be created.
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "daemon-tls.crt"))
	require.True(t, os.IsNotExist(statErr), "HTTP-only daemon must generate no TLS cert")

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + info.Addr

	// --- REST: correct token → 200 -----------------------------------------
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := client.Do(req)
	require.NoError(t, err, "authorized request must succeed")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// --- REST: no token → 401 ----------------------------------------------
	req, err = http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "missing token must be rejected")
	_ = resp.Body.Close()

	// --- REST: wrong token → 401 -------------------------------------------
	req, err = http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer not-the-real-token")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "wrong token must be rejected")
	_ = resp.Body.Close()

	// --- WS: correct token via ?access_token= → upgrades + streams ---------
	wsBase := "ws://" + info.Addr
	dialOpts := &websocket.DialOptions{HTTPClient: client}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsBase+"/v1/events?"+agentproto.AccessTokenQueryParam+"="+info.Token, dialOpts)
	require.NoError(t, err, "authorized WS handshake must upgrade")
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Prove the stream is live: publish repeatedly (absorb subscribe race) and
	// read one event back over the token-gated socket.
	ev, err := agentproto.NewEvent(agentproto.EventTaskCreated, nil)
	require.NoError(t, err)
	got := make(chan agentproto.MessageType, 1)
	go func() {
		if msg, rerr := agentproto.ReadMessage(ctx, conn); rerr == nil {
			if typ, terr := agentproto.MessageTypeOf(msg.Text); terr == nil {
				got <- typ
			}
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for streaming := true; streaming; {
		select {
		case typ := <-got:
			require.Equal(t, string(agentproto.EventTaskCreated), string(typ))
			streaming = false
		case <-ticker.C:
			m.events.publish(ev)
		case <-ctx.Done():
			t.Fatal("authorized WS client never received a published event")
		}
	}

	// --- WS: no token → handshake fails (401 pre-empts the upgrade) ---------
	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	badConn, _, err := websocket.Dial(badCtx, wsBase+"/v1/events", dialOpts)
	if badConn != nil {
		_ = badConn.Close(websocket.StatusNormalClosure, "")
	}
	require.Error(t, err, "unauthenticated WS handshake must be rejected")
}

// TestTCPListener_ServesWebShellUnauthed is the PR2 payoff: the HTTP TCP listener
// serves the embedded SPA shell WITHOUT a token (you cannot paste a token into a
// page that will not load), while the /v1/ data plane stays token-gated on the
// exact same listener. It also asserts the strict CSP the static handler sets.
func TestTCPListener_ServesWebShellUnauthed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}
	// Strict policy (loopback NOT exempt) so the "/v1 stays token-gated" assertions
	// below still hold over a real loopback socket — this test's subject is the
	// UNAUTHENTICATED static shell, which is policy-independent.
	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, tokenGatePolicy{}, withWebShell)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + info.Addr

	// --- Static shell: NO token → 200 + index.html + strict CSP ------------
	resp, err := client.Get(baseURL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the shell must load without a token")
	require.Contains(t, string(body), `id="app"`)
	require.Equal(t, "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self' https: http:", resp.Header.Get("Content-Security-Policy"))

	// The JS bundle is likewise reachable unauthenticated.
	resp, err = client.Get(baseURL + "/af-web.js")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the bundle must load without a token")

	// --- API on the SAME listener: still token-gated -----------------------
	resp, err = client.Get(baseURL + "/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the data plane stays gated")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err = client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "a valid token still reaches the API")
}

// TestTCPListener_LoopbackExempt is the #1696 end-to-end payoff: the daemon's
// web policy (loopback exempt) over a REAL HTTP TCP socket on 127.0.0.1 serves the
// /v1 data plane with NO token, because the peer is loopback. It proves the
// exemption holds through the full webShellHandler + gate stack, not just the
// handler unit test.
func TestTCPListener_LoopbackExempt(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}
	// The daemon's real posture: loopback exempt, token still required for the
	// (here unreachable) network peers.
	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, tokenGatePolicy{loopbackExempt: true}, withWebShell)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + info.Addr

	// --- /v1 data plane over loopback with NO token → 200 (exempt) ----------
	resp, err := client.Get(baseURL + "/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "loopback peer must reach the API with no token")

	// --- Unauthenticated auth-info probe → auth_required=false for loopback --
	resp, err = client.Get(baseURL + "/v1/auth-info")
	require.NoError(t, err)
	var env struct {
		Data struct {
			AuthRequired bool `json:"auth_required"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	_ = resp.Body.Close()
	require.Nil(t, env.Error)
	require.False(t, env.Data.AuthRequired, "a loopback client must be told it needs no token")
}

// TestTCPListener_RequireLoopbackToken pins require_loopback_token=true: the
// loopback exemption is withdrawn (policy loopbackExempt=false, the shape
// startHTTPServer builds from the config flag), so a same-machine peer must
// present the token exactly like a network peer. This is the shared/multi-user
// tighten-up — it proves a local process WITHOUT the token is rejected while the
// same request WITH the token is allowed, through the full gate stack.
func TestTCPListener_RequireLoopbackToken(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}
	// require_loopback_token=true ⇒ loopbackExempt=false. Token still enforced
	// for the (unreachable here) network peers too — this only removes the
	// loopback shortcut.
	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, tokenGatePolicy{loopbackExempt: false}, withWebShell)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()
	require.NotEmpty(t, info.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + info.Addr

	// --- loopback with NO token → 401 (exemption withdrawn) -----------------
	resp, err := client.Get(baseURL + "/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"require_loopback_token=true must reject a loopback peer with no token")

	// --- the auth-info probe now reports auth_required=true for loopback ----
	resp, err = client.Get(baseURL + "/v1/auth-info")
	require.NoError(t, err)
	var env struct {
		Data struct {
			AuthRequired bool `json:"auth_required"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	_ = resp.Body.Close()
	require.Nil(t, env.Error)
	require.True(t, env.Data.AuthRequired, "a loopback client must now be told it needs a token")

	// --- loopback WITH the correct token → 200 ------------------------------
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err = client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a loopback peer presenting the token must be allowed")
}

// TestStartHTTPServer_WebOnByDefault pins the bundled-web-UI default: the plain
// DefaultConfig() carries the loopback listen_addr, so startHTTPServer binds the
// HTTP TCP listener with no config at all — and generates NO cert on disk.
func TestStartHTTPServer_WebOnByDefault(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	require.Equal(t, "127.0.0.1:8443", cfg.ListenAddr,
		"default config must serve the web UI on loopback")
	// Use :0 so the test never races the real 8443 or another daemon; the point
	// under test is that a non-empty default triggers the bind, not the port.
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	// HTTP-only: no TLS material is ever materialized, even with the listener up.
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "daemon-tls.crt"))
	require.True(t, os.IsNotExist(statErr), "no cert should be generated for the HTTP listener")
}

// TestStartHTTPServer_NoTCPWhenDisabled pins the opt-out: an explicit
// listen_addr="" leaves ONLY the unix socket bound and opens no TCP port —
// byte-identical to the pre-Phase-3 daemon.
func TestStartHTTPServer_NoTCPWhenDisabled(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "" // explicit opt-out disables the web server
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	require.NoError(t, closeHTTP())
}

// TestStartHTTPServer_BindConflictNonFatal pins robustness item 3: when the web
// listener cannot bind (here, an already-in-use address, the port-race case),
// startHTTPServer still returns a live daemon — the web server is skipped, never
// crashes the control plane. The bound unix socket keeps working.
func TestStartHTTPServer_BindConflictNonFatal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Occupy a port, then point the daemon's web listener straight at it so the
	// bind is guaranteed to fail with EADDRINUSE.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()

	cfg := config.DefaultConfig()
	cfg.ListenAddr = blocker.Addr().String()
	m, err := NewManager(cfg)
	require.NoError(t, err)

	// The daemon comes up despite the doomed web bind.
	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err, "a web-port conflict must never fail daemon startup")
	defer func() { require.NoError(t, closeHTTP()) }()

	// The unix control socket is still serving — Ping over it succeeds.
	socketPath, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	resp, err := httpClient.Get("http://unix/v1/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// freeNetworkPort reserves a port on all interfaces, then releases it, so a test
// can assert on an address NOTHING is listening on. Binding 0.0.0.0 (rather than
// a 127.x address, which net.IP.IsLoopback covers in full) is what makes the
// address genuinely non-loopback to the guard under test.
func freeNetworkPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// captureWarnings redirects log.WarningLog into a buffer for the duration of a
// test and returns a reader for what was written. The logger vars are package
// globals, so it restores the previous one — a leaked writer would send every
// later test's warnings into a dead buffer.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.WarningLog
	log.WarningLog = stdlog.New(&buf, "WARNING: ", 0)
	t.Cleanup(func() { log.WarningLog = prev })
	return buf.String
}

// TestStartHTTPServer_BindsUnauthenticatedNetworkListener is the #2168 Phase 0
// lock, and it REVERSES the #2090 assertion that stood here before: the port must
// be OPEN and serving.
//
// The owner's decision, verbatim: "just allow binding to 0.0.0.0 without a token.
// Assume users are safe and will do the right thing; if it becomes a problem I
// will check up on them." So this is not a regression to be re-fixed — it is the
// deliberate posture, and a future change that stops binding here breaks a
// documented configuration.
//
// The exposure is still SAID, exactly once. Log spam is the failure mode of a
// warning that replaces a refusal, so "once" is asserted, not assumed: a per-
// request or per-connection warning would fire again on the requests below.
func TestStartHTTPServer_BindsUnauthenticatedNetworkListener(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnings := captureWarnings(t)

	addr := freeNetworkPort(t)
	cfg := config.DefaultConfig()
	cfg.ListenAddr = addr // network-bound…
	cfg.RequireToken = false
	// …and require_loopback_token cannot stand in for the real token: it is inert
	// while require_token is false, so this peer is genuinely unauthenticated.
	cfg.RequireLoopbackToken = true

	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err, "an exposed posture must never take the daemon down")
	defer func() { require.NoError(t, closeHTTP()) }()

	// The property: it BINDS. Pre-#2168 this dial failed (connection refused)
	// because startHTTPServer skipped the listener entirely.
	conn, dialErr := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, dialErr, "a tokenless network bind must be served, not skipped")
	require.NoError(t, conn.Close())

	// And it SERVES: a caller with no credentials is answered, not 401'd. That is
	// the whole point of require_token = false, and asserting the bind alone would
	// pass against a listener that authenticated everyone.
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 3; i++ {
		resp, err := client.Post("http://"+addr+"/v1/Snapshot", "application/json", nil)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.NotEqual(t, http.StatusUnauthorized, resp.StatusCode,
			"require_token = false means no peer is challenged — including this one")
	}

	// Said once, and worth reading: the address, what a peer can do with it, and
	// how to turn auth on. The three requests above must not have added copies.
	got := warnings()
	require.Equal(t, 1, strings.Count(got, "reachable from the network"),
		"the exposure warning must be emitted exactly once per daemon start, not per request:\n%s", got)
	require.Contains(t, got, addr)
	require.Contains(t, got, "DeliverPrompt")
	require.Contains(t, got, "require_token = true")
}

// TestStartHTTPServer_NetworkListenerServesWithToken pins that #2168 Phase 0
// changed only the tokenless side. An operator who sets require_token = true
// still gets their remote web UI — and it answers 401 to a caller with no
// credentials, which is the enforcement path this relaxation must not touch.
func TestStartHTTPServer_NetworkListenerServesWithToken(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	addr := freeNetworkPort(t)
	cfg := config.DefaultConfig()
	cfg.ListenAddr = addr
	cfg.RequireToken = true

	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Post("http://"+addr+"/v1/Snapshot", "application/json", nil)
	require.NoError(t, err, "an authenticated network bind must still be served")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"the token must actually be enforced — this is what makes the network bind safe")
}
