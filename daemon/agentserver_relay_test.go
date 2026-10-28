package daemon

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func startRelayAgentServer(t *testing.T) (info AgentServerInfo, rel session.LoopbackRelayer, as session.AgentServer) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), title: "probe", events: newEventsHub()}
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	closeTCP, tcp, err := startTCPListener(hs.newMux(), cfg.ListenAddr, cfg, tokenGatePolicy{}, withoutWebShell, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeTCP.close() })

	as, err = session.NewRemoteAgentServer(session.AgentServerEndpoint{URL: "http://" + tcp.Addr, Token: tcp.Token}, "probe")
	require.NoError(t, err)
	rel, ok := as.(session.LoopbackRelayer)
	require.True(t, ok, "the remote agent-server must implement LoopbackRelayer")
	return AgentServerInfo{Addr: tcp.Addr, Token: tcp.Token}, rel, as
}

func TestAgentServerRelayCarriesAnHTTPServerOnWorkspaceLoopback(t *testing.T) {
	_, rel, _ := startRelayAgentServer(t)

	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB: many frames
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return rel.DialLoopback(ctx, port)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://workspace.invalid/", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got), "relayed body must match byte for byte (got %d, want %d)", len(got), len(payload))
}

func TestAgentServerRelayRefusesAClosedPort(t *testing.T) {
	_, rel, _ := startRelayAgentServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = rel.DialLoopback(ctx, port)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing is listening on 127.0.0.1:"+strconv.Itoa(port))
}

func TestAgentServerRelayRejectsBadPortsAndMissingToken(t *testing.T) {
	info, rel, _ := startRelayAgentServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, port := range []int{0, -1, 65536} {
		_, err := rel.DialLoopback(ctx, port)
		require.Error(t, err, "port %d", port)
	}

	for _, q := range []string{"", "?port=abc", "?port=0", "?port=99999", "?port=80&host=10.0.0.1"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+info.Addr+"/v1/agent/relay"+q, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+info.Token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		if q == "?port=80&host=10.0.0.1" {
			// The host parameter is ignored: this is a relay to 127.0.0.1:80, which
			// is not a websocket upgrade here and therefore refused either way.
			require.GreaterOrEqual(t, resp.StatusCode, 400)
			continue
		}
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "query %q", q)
	}

	req, err := http.NewRequest(http.MethodGet, "http://"+info.Addr+"/v1/agent/relay?port=80", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no token must be rejected")
}

func TestAgentServerRelayClosesWithTheSession(t *testing.T) {
	_, rel, as := startRelayAgentServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := rel.DialLoopback(ctx, ln.Addr().(*net.TCPAddr).Port)
	require.NoError(t, err)

	require.NoError(t, as.Kill(false))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "i/o timeout", "Kill must close open relays, not leave them hanging")

	_, err = rel.DialLoopback(ctx, ln.Addr().(*net.TCPAddr).Port)
	require.Error(t, err, "a killed session must not open new relays")
}
