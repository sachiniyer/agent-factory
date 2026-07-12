package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// pinnedTLSConfig builds a client TLS config that trusts ONLY the daemon's
// self-signed cert (read from disk), mirroring the fingerprint-pin PR4 will do.
// The cert carries a 127.0.0.1 SAN, so verifying against 127.0.0.1 succeeds.
func pinnedTLSConfig(t *testing.T, certPath string) *tls.Config {
	t.Helper()
	pem, err := os.ReadFile(certPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pem), "load self-signed cert into pool")
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// TestTCPListener_TLS_TokenRoundTrip is the PR3 payoff: a real TLS TCP listener
// on loopback that REQUIRES the bearer token. It proves REST + WS both accept a
// correct token over https/wss and reject a missing/wrong one, with the client
// pinning the self-signed cert.
func TestTCPListener_TLS_TokenRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0" // :0 ⇒ the kernel picks a free port
	m, err := NewManager(cfg)
	require.NoError(t, err)

	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}
	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()

	require.NotEmpty(t, info.Token)
	require.True(t, info.SelfSigned)
	require.True(t, strings.HasPrefix(info.Fingerprint, "sha256:"))

	// The self-signed material lives in the af home; pin it.
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: pinnedTLSConfig(t, dir+"/"+daemonTLSCertFileName)},
	}
	baseURL := "https://" + info.Addr

	// --- REST: correct token → 200 -----------------------------------------
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := client.Do(req)
	require.NoError(t, err, "TLS handshake + authorized request must succeed")
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
	wsBase := "wss://" + info.Addr
	dialOpts := &websocket.DialOptions{HTTPClient: client}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsBase+"/v1/events?"+agentproto.AccessTokenQueryParam+"="+info.Token, dialOpts)
	require.NoError(t, err, "authorized WS handshake must upgrade")
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Prove the stream is live: publish repeatedly (absorb subscribe race) and
	// read one event back over the encrypted, token-gated socket.
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

// TestStartHTTPServer_NoTCPByDefault pins the off-by-default guarantee: with an
// empty ListenAddr (the default) startHTTPServer binds ONLY the unix socket and
// no TCP port is opened — byte-identical to the pre-Phase-3 daemon.
func TestStartHTTPServer_NoTCPByDefault(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.Empty(t, m.cfg.ListenAddr, "default config must leave the TCP listener off")

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	require.NoError(t, closeHTTP())

	// No self-signed TLS material is generated when the listener never binds.
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	_, statErr := os.Stat(dir + "/" + daemonTLSCertFileName)
	require.True(t, os.IsNotExist(statErr), "no cert should be generated when TCP is off")
}
