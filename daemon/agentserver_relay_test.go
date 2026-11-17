package daemon

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
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
	// The closed port refuses on every platform: a real listener accepts the
	// workspace conn and hands it to the test, which resets it with SO_LINGER
	// 0 — but only after the relay's dial has returned, so the RST can never
	// race the connect() itself (Codex on #7467: sent from the accept loop it
	// occasionally lands inside DialContext, and the dial fails "nothing is
	// listening"). deadPortAddr can't stand in here — its bound,
	// never-listening socket draws an immediate RST on Linux but has its SYN
	// dropped on darwin, where the dial hangs to its deadline instead of
	// refusing (#7466). Holding the listener for the whole test keeps the
	// release-and-rebind race #7460 removed out too.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			select {
			case accepted <- c:
			default:
				_ = c.Close()
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := rel.DialLoopback(ctx, port)
	require.NoError(t, err)
	defer conn.Close()

	// The conn the relay just opened in the workspace: it was established
	// before the handshake reply reached us, so it is already in `accepted`
	// or about to be. Resetting it now hits an established conn — always the
	// open relay, never the dial.
	var target net.Conn
	select {
	case target = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the opened relay produced no workspace connection")
	}
	require.NoError(t, target.(*net.TCPConn).SetLinger(0))
	require.NoError(t, target.Close())

	// The relay must tear the conn down on the reset — promptly, and only on
	// the reset. The read carries no deadline of its own: a watchdog ends the
	// test instead, so a read that would only finish on a deadline (the
	// websocket NetConn reports those as context errors, not "i/o timeout")
	// is a failure, never a pass (Codex on #7467).
	readErr := make(chan error, 1)
	go func() {
		_, rerr := conn.Read(make([]byte, 1))
		readErr <- rerr
	}()
	select {
	case err = <-readErr:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the reset never reached the relayed conn")
	}
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
