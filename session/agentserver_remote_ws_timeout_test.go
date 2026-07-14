package session

import (
	"context"
	"net"
	"testing"
	"time"
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
