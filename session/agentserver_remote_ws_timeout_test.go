package session

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestRemoteAgentDialStream_StalledHandshakeTimesOut is the #1730 guard on the
// INTERNAL daemon→agent-server hop: an agent-server that accepts the TCP
// connection but never answers the WS upgrade must make dialStream error out
// (via remoteAgentWSHandshakeTimeout) instead of hanging the daemon's capture
// goroutine forever. Plain HTTP has no TLS-handshake timeout to lean on, so this
// bound is what preserves the protection on this hop.
func TestRemoteAgentDialStream_StalledHandshakeTimesOut(t *testing.T) {
	// A listener that accepts TCP but never writes the 101 response — the exact
	// half-open upgrade the bound must catch. Every accepted conn is held open.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conns := make(chan net.Conn, 16)
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				close(conns)
				return
			}
			conns <- c // hold it open, never respond — stall the upgrade
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for c := range conns {
			_ = c.Close()
		}
	})

	// Shrink the handshake bound so the test proves it FIRES without waiting the
	// full budget, restoring it after.
	orig := remoteAgentWSHandshakeTimeout
	remoteAgentWSHandshakeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { remoteAgentWSHandshakeTimeout = orig })

	rc, err := newRemoteAgentClient(AgentServerEndpoint{
		URL:   "http://" + ln.Addr().String(),
		Token: "tok",
	}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}

	errc := make(chan error, 1)
	go func() {
		_, e := rc.dialStream(context.Background(), 0)
		errc <- e
	}()
	select {
	case e := <-errc:
		if e == nil {
			t.Fatal("stalled agent-server WS upgrade: want an error, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: dialStream did not return on a stalled agent-server upgrade (#1730 regression on the internal hop)")
	}
}

func TestRemoteAgentDialStream_StalledRequestWriteTimesOut(t *testing.T) {
	origDial, origHandshake := remoteAgentDialTimeout, remoteAgentWSHandshakeTimeout
	remoteAgentDialTimeout = 250 * time.Millisecond
	remoteAgentWSHandshakeTimeout = 250 * time.Millisecond
	t.Cleanup(func() {
		remoteAgentDialTimeout, remoteAgentWSHandshakeTimeout = origDial, origHandshake
	})

	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: "http://pipe.invalid", Token: "tok"}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}
	client, peer := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
	})
	rc.transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return client, nil
	}

	errc := make(chan error, 1)
	go func() {
		_, err := rc.dialStream(context.Background(), 0)
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("stalled agent-server WebSocket request write unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HANG: agent-server dial did not return while the peer stopped reading the upgrade request")
	}
}

// TestRemoteAgentDialStream_SlowTCPDialKeepsFullHandshakeBudget audits #2670's
// adjacent daemon-to-agent-server stream dial. Its TCP connect and WebSocket
// upgrade must receive independent timeout budgets too.
func TestRemoteAgentDialStream_SlowTCPDialKeepsFullHandshakeBudget(t *testing.T) {
	orig := remoteAgentWSHandshakeTimeout
	remoteAgentWSHandshakeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { remoteAgentWSHandshakeTimeout = orig })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "tok"}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}
	transport := rc.httpClient.Transport.(*http.Transport)
	dial := transport.DialContext
	dialDelay := 2 * remoteAgentWSHandshakeTimeout
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		timer := time.NewTimer(dialDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return dial(ctx, network, address)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	conn, err := rc.dialStream(context.Background(), 0)
	if err != nil {
		t.Fatalf("TCP dial used the WebSocket handshake budget: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

type closeNotifyAgentConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeNotifyAgentConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestRemoteAgentDialStream_RejectedUpgradeClosesPerDialTransport(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upgrade rejected", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "tok"}, "probe")
	if err != nil {
		t.Fatalf("newRemoteAgentClient: %v", err)
	}
	closed := make(chan struct{})
	dial := rc.transport.DialContext
	rc.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &closeNotifyAgentConn{Conn: conn, closed: closed}, nil
	}

	if _, err := rc.dialStream(context.Background(), 0); err == nil {
		t.Fatal("rejected agent-server WebSocket upgrade unexpectedly succeeded")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("rejected agent-server upgrade left the per-dial transport connection idle")
	}
}
