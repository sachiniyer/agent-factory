package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The web-tab preview listener (#1856), step 1: config + a SECOND TCP listener
// that opens a port but serves nothing yet. These tests pin the step-1 contract —
// disabled by default, binds when configured, serves NEITHER the control API nor
// content, and a bind failure never touches the daemon — so a later step only has
// to add routing, not re-establish any of this.

// previewHTTPClient dials a concrete host:port over plain HTTP. The preview
// listener is a real TCP socket, so a real client is the honest way to prove what
// it does and does not serve.
func previewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}
}

// TestStartHTTPServer_NoPreviewWhenDisabled pins the default: preview_listen_addr
// is empty out of the box, so no second port opens and the lifecycle reports it
// unconfigured. This is the "no behavior change until opt-in" guarantee.
func TestStartHTTPServer_NoPreviewWhenDisabled(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	require.Equal(t, "", cfg.PreviewListenAddr, "preview must be disabled by default")
	// Keep the control listener off the real 8443 without disabling it.
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	listeners := m.lifecycle.snapshot().listeners
	require.False(t, listeners.PreviewConfigured, "an empty preview_listen_addr configures no preview listener")
	require.False(t, listeners.PreviewBound, "nothing must bind when preview is disabled")
	require.Empty(t, listeners.PreviewBoundAddr)
}

// TestStartHTTPServer_PreviewBindsButServesNothing is the core invariant, now
// through the step-2 credential (#1856). A configured preview_listen_addr binds a
// second port, the lifecycle reports it bound, and that port serves NEITHER the
// daemon control API NOR any content — and it authenticates its OWN ephemeral
// credential, not the daemon bearer.
//
// The separation is the headline: the daemon token is REJECTED on the preview port
// (it is not the preview credential), while the preview token authenticates and
// then still finds no content. Both prove the preview origin is a distinct,
// non-control surface.
func TestStartHTTPServer_PreviewBindsButServesNothing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"        // control listener, tokenless loopback default
	cfg.PreviewListenAddr = "127.0.0.1:0" // preview listener on its own ephemeral port
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewConfigured)
	require.True(t, listeners.PreviewBound, "a configured preview_listen_addr must bind")
	require.NotEmpty(t, listeners.PreviewBoundAddr)
	require.NotEqual(t, listeners.TCPBoundAddr, listeners.PreviewBoundAddr,
		"the preview listener is a SECOND socket, distinct from the control listener")

	previewAddr := listeners.PreviewBoundAddr
	client := previewHTTPClient()

	// The DAEMON token is the wrong credential for the preview origin: the gate
	// rejects it. This is the separation — a control-plane credential cannot reach
	// the preview surface at all.
	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	tok := previewTabToken(m.previewSecret, "s1", "t1")
	require.NotEqual(t, daemonToken, tok, "the preview credential must be distinct from the daemon bearer")

	snapWithDaemon := previewGet(t, client, "http://"+previewAddr+"/v1/Snapshot", daemonToken)
	require.Equal(t, http.StatusUnauthorized, snapWithDaemon,
		"the daemon token must NOT authorize the preview origin — the credentials are separate")

	// A control-API path on the preview origin has NO tab identity, so the per-tab
	// gate can derive no expected token and rejects it outright — the preview origin
	// exposes no control API, and /v1/Snapshot never even reaches a dispatch. Even a
	// real per-tab token cannot reach it: the token authenticates a TAB's route, and
	// /v1/Snapshot is not one.
	snapWithPreview := previewGet(t, client, "http://"+previewAddr+"/v1/Snapshot", tok)
	require.Equal(t, http.StatusUnauthorized, snapWithPreview,
		"a non-tab /v1 path on the preview origin has no per-tab credential — it must 401, never dispatch")

	// A real tab route, authenticated with that tab's token, is the ONLY thing that
	// gets past the gate — and finds no content yet (404), proving the gate works and
	// the origin still serves nothing.
	assetWithPreview := previewGet(t, client, "http://"+previewAddr+"/v1/webtab/s1/t1/asset.js", tok)
	require.Equal(t, http.StatusNotFound, assetWithPreview,
		"authenticated on its own tab route, the preview origin still serves no content (404, not a dispatch)")

	// The non-/v1 404 sits OUTSIDE the gate (previewShell answers a root request
	// UNAUTHENTICATED, before the token is consulted — so this deliberately sends no
	// token). It must carry the PREVIEW origin's own message: not the SPA, and
	// crucially NOT the agent-server's text, which names the control-plane address
	// and would advertise it to an unauthenticated peer.
	rootResp, err := client.Get("http://" + previewAddr + "/")
	require.NoError(t, err)
	rootBody, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	require.Equal(t, http.StatusNotFound, rootResp.StatusCode,
		"the preview origin serves no content — root must 404, not return the SPA")
	require.Contains(t, string(rootBody), "preview origin",
		"the preview port must answer with its own message")
	require.NotContains(t, string(rootBody), "localhost:8443",
		"the preview port must NOT advertise the control-plane address to an unauthenticated peer")
	require.NotContains(t, string(rootBody), "agent-server",
		"the preview port is not the agent-server; it must not borrow that message")

	// The control listener, by contrast, DOES serve the API (tokenless loopback
	// default), so the two ports are genuinely different surfaces.
	ctrlResp, err := client.Get("http://" + listeners.TCPBoundAddr + "/v1/health")
	require.NoError(t, err)
	defer func() { _ = ctrlResp.Body.Close() }()
	require.Equal(t, http.StatusOK, ctrlResp.StatusCode,
		"the control listener still serves its API — the preview port is the odd one out, by design")
}

// mustTokenPath resolves the daemon token path or fails the test.
func mustTokenPath(t *testing.T) string {
	t.Helper()
	p, err := TokenPath()
	require.NoError(t, err)
	return p
}

// previewGet issues a GET carrying token as a Bearer header and returns the status
// code. A "" token sends no Authorization header.
func previewGet(t *testing.T, client *http.Client, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestStartHTTPServer_PreviewBindConflictNonFatal mirrors the control listener's
// robustness contract for the preview port: when it cannot bind (a port already
// in use), the daemon still comes up and the control plane is untouched — a
// second web port must never be able to take the daemon down.
func TestStartHTTPServer_PreviewBindConflictNonFatal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Occupy a port, then point the preview listener straight at it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = blocker.Addr().String()
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err, "a preview-port conflict must never fail daemon startup")
	defer func() { require.NoError(t, closeHTTP()) }()

	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewConfigured, "the address was configured even though the bind failed")
	require.False(t, listeners.PreviewBound, "a doomed preview bind must report not-bound")

	// The control listener is unaffected — it bound and serves.
	require.True(t, listeners.TCPBound, "the control listener must survive a preview-bind failure")
	client := previewHTTPClient()
	resp, err := client.Get("http://" + listeners.TCPBoundAddr + "/v1/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
