package apiclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSnapshotWithAlarmsCtx_BoundsWedgedLocalRead is the live-wedged-socket
// reproduction of the #6045 root cause — the report's evidence #1. A wedged
// local daemon (accepts the unix-socket connection, never writes a response)
// would, without a deadline riding the transport read, park the caller forever:
// the http.Client sets no Timeout and the daemon's http.Server sets only
// ReadHeaderTimeout, so once the connection is accepted the read is unbounded.
//
// SnapshotWithAlarmsCtx(ctx, req) bounds the read: the deadline fires at the
// caller's ctx, returning context.DeadlineExceeded, instead of hanging until
// the process is killed. Runs against a REAL unix socket — not a stub — so the
// transport itself (the dial-bounded-only shape apiclient/client.go documents)
// is what's under test.
func TestSnapshotWithAlarmsCtx_BoundsWedgedLocalRead(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	// A wedged handler: accept the request, never reply. ReadHeaderTimeout
	// mirrors the daemon's own transport (the report's setup).
	wedged := make(chan struct{})
	var requestSeen atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, _ *http.Request) {
		requestSeen.Store(true)
		<-wedged // never returns during the test body
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		close(wedged)
		_ = srv.Close()
		_ = ln.Close()
	})

	c := NewWithSocket(sockPath)
	defer c.CloseIdleConnections()

	// The bounded call must return at the ctx deadline, not park forever.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.SnapshotWithAlarmsCtx(ctx, daemon.SnapshotRequest{RepoID: "r"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("SnapshotWithAlarmsCtx against a wedged daemon returned nil error after %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ctx.DeadlineExceeded, got %v after %s", err, elapsed)
	}
	// The deadline must actually fire — not return instantly. Give it the
	// full bound (no false-failure in the dial, which is bounded at 250ms and
	// will fail/refuse fast against a wedged socket — but here the socket
	// accepts, so the dial succeeds and the read parks until the deadline).
	if elapsed < 150*time.Millisecond {
		t.Fatalf("SnapshotWithAlarmsCtx returned in %s, expected to wait up to the 200ms deadline against a wedged read", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("SnapshotWithAlarmsCtx returned in %s, expected the deadline to bound the wedged read much sooner than the TUI's event-loop tolerance", elapsed)
	}
	if !requestSeen.Load() {
		t.Fatalf("the request was never served; the dial must reach the wedged handler for the bound to ride the transport read")
	}
	t.Logf("wedged read bounded: SnapshotWithAlarmsCtx(200ms deadline) returned after %s with %v", elapsed, err)
}
