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

// TestSnapshotWithAlarms_NoCtxStaysUnboundedAgainstWedgedRead documents the
// pre-fix shape against the same wedged socket: SnapshotWithAlarms (the no-ctx
// wrapper, used by the production code path the report names) passes
// context.Background() all the way down, so the read has NO deadline. A wedged
// daemon would park it forever. This test PROVES the pre-fix hang by capping
// the wait externally: if the call were still unbounded it would not return
// before the external deadline fires; but it returns ~only once the wedged
// handler is un-wedged — confirming the read's unboundedness — without a long
// stall, exactly as the report's "still blocked after 6s" evidence shows.
func TestSnapshotWithAlarms_NoCtxStaysUnboundedAgainstWedgedRead(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	wedged := make(chan struct{})
	var requestSeen atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, _ *http.Request) {
		requestSeen.Store(true)
		<-wedged
		// Once un-wedged, write a malformed body so the read returns — proving
		// it returned BECAUSE the server wrote, not because of a deadline. This
		// is exactly what the report's "still blocked after 6.0s — only ever
		// returned when the test un-wedged the server" observed.
		_, _ = w.Write([]byte("{"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		select {
		case <-wedged:
		default:
			close(wedged)
		}
		_ = srv.Close()
		_ = ln.Close()
	})

	c := NewWithSocket(sockPath)
	defer c.CloseIdleConnections()

	// Run an UNBOUNDED call in a goroutine. If #6045's fix worked (no-ctx path
	// is documented as unbounded for off-loop callers, context.Background all
	// the way down) it should stay parked until wedged is closed — NOT return
	// within any short test-imposed deadline from a real transport bound.
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := c.SnapshotWithAlarms(daemon.SnapshotRequest{RepoID: "r"})
		done <- result{err, time.Since(start)}
	}()

	// It must NOT return within 1s — the only thing that can end the parked
	// read is the wedged handler being released (the no-ctx call has no
	// deadline). If it returned fast, the local transport would be securable,
	// contradicting #6045's root cause and the apiclient.Client documentation.
	select {
	case r := <-done:
		t.Fatalf("SnapshotWithAlarms (context.Background) returned in %s with %v against a wedged daemon — the local unix-socket read was supposed to be unbounded (#6045 root cause)", r.elapsed, r.err)
	case <-time.After(1 * time.Second):
	}

	// Now un-wedge the server. The parked read should return at once — proving
	// that without ctx's deadline it would park "until the process is killed"
	// against a permanently-wedged daemon (the report's failure mode).
	close(wedged)
	select {
	case r := <-done:
		if r.elapsed < 1*time.Second {
			t.Fatalf("returned in %s, before the 1s wait that preceded the un-wedge — racing a wedged read would invalidate the bound comparison", r.elapsed)
		}
		t.Logf("unbounded read confirmed: SnapshotWithAlarms stayed parked for %s until the wedged handler was released (err=%v)", r.elapsed, r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("SnapshotWithAlarms never returned even after the wedged handler was un-wedged")
	}
}
