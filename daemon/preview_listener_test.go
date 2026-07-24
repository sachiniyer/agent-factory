package daemon

import (
	"context"
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

// TestStartHTTPServer_PreviewBindsButServesNothing is the core step-1 invariant.
// A configured preview_listen_addr binds a second port, the lifecycle reports it
// bound, and — critically — that port serves NEITHER the daemon control API NOR
// any content. The whole reason for a separate origin is that it must never carry
// the control plane, so a request for /v1/Snapshot on the preview port must not
// dispatch.
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

	// Present a valid daemon token so the strict preview gate cannot be the reason
	// a control route is unreachable — this proves the ROUTE is absent, not merely
	// gated. With the token accepted, the empty preview mux 404s /v1/Snapshot.
	tokenPath, err := TokenPath()
	require.NoError(t, err)
	token, err := LoadToken(tokenPath)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "http://"+previewAddr+"/v1/Snapshot", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the preview origin must NOT serve the daemon control API — /v1/Snapshot must 404, not dispatch")

	// And it serves no browser shell either (withoutWebShell): a root request 404s
	// rather than returning the SPA.
	rootResp, err := client.Get("http://" + previewAddr + "/")
	require.NoError(t, err)
	defer func() { _ = rootResp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, rootResp.StatusCode,
		"the preview origin serves no content yet — root must 404, not return the SPA")

	// The control listener, by contrast, DOES serve the API (tokenless loopback
	// default), so the two ports are genuinely different surfaces.
	ctrlResp, err := client.Get("http://" + listeners.TCPBoundAddr + "/v1/health")
	require.NoError(t, err)
	defer func() { _ = ctrlResp.Body.Close() }()
	require.Equal(t, http.StatusOK, ctrlResp.StatusCode,
		"the control listener still serves its API — the preview port is the odd one out, by design")
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
