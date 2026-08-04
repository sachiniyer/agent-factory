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

// The web-tab preview listener (#1856): config + a SECOND TCP listener whose ONLY
// surface is the per-tab preview origins. These tests pin the listener contract —
// disabled by default, binds when configured, serves neither the control API nor
// anything on a host that is not a per-tab origin, and a bind failure never touches
// the daemon.

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

// TestStartHTTPServer_PreviewServesPreviewsOnly is the listener's core contract
// (#1856). A configured preview_listen_addr binds a SECOND port; that port serves
// NEITHER the daemon control API nor anything at all on a host that is not one of
// its per-tab preview origins; and it authenticates its OWN per-tab credential —
// the origin's hostname — not the daemon bearer.
//
// The separation is the headline: the daemon token buys nothing here, and no path
// on the bare bound address is a route, because on this listener a tab owns its
// origin's WHOLE path space and only the Host names a tab.
func TestStartHTTPServer_PreviewServesPreviewsOnly(t *testing.T) {
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

	// The DAEMON token is the wrong credential for the preview origin, and the bare
	// bound address is not a preview origin at all — so a control-API path there is
	// answered by the preview origin's own explanatory 404, never dispatched.
	daemonToken, err := LoadToken(mustTokenPath(t))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound,
		previewGet(t, client, "http://"+previewAddr+"/v1/Snapshot", daemonToken),
		"the preview port must expose no control API, and the daemon token must buy nothing on it")

	// Same for the mirrored app-origin route: that path belongs to the CONTROL
	// listener, and on the preview listener it names no tab, because only the Host does.
	require.Equal(t, http.StatusNotFound,
		previewGet(t, client, "http://"+previewAddr+"/v1/webtab/s1/t1/asset.js", daemonToken),
		"the app-origin mirror path is not a route on the preview listener")

	// The non-preview-host 404 sits OUTSIDE the gate (previewShellHandler answers it
	// UNAUTHENTICATED, before any credential is consulted — so this deliberately sends
	// none). It must carry the PREVIEW origin's own message: not the SPA, and
	// crucially NOT the agent-server's text, which names the control-plane address
	// and would advertise it to an unauthenticated peer.
	rootResp, err := client.Get("http://" + previewAddr + "/")
	require.NoError(t, err)
	rootBody, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	require.Equal(t, http.StatusNotFound, rootResp.StatusCode,
		"the bare preview address serves nothing — root must 404, not return the SPA")
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
