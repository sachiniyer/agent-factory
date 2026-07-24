package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// wedgedDaemonSocket binds a Unix-socket HTTP server that accepts every
// connection and never answers, releasing at cleanup. No real daemon, no real AF
// home: the socket lives in t.TempDir() and EnsureDaemon is bypassed by the
// withDaemonHTTP seam below, so this test can never touch the box's daemon.
func wedgedDaemonSocket(t *testing.T) string {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	block := make(chan struct{})
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { close(block); _ = srv.Close() })
	return sockPath
}

// TestKillSessionThroughDaemon_WedgedDaemon_ReturnsActionableError is the
// regression lock for the reported symptom: pressing D on a session hung forever
// instead of doing anything.
//
// It drives the REAL killSessionThroughDaemon — the seam killInstanceCmd calls —
// so the production bound is what's under test, not a helper. Only withDaemonHTTP
// is faked, and only to skip EnsureDaemon and point the client at a wedged socket;
// the ctx the fix threads still has to survive that closure for the bound to fire.
//
// Against master this test HANGS (killSessionThroughDaemon passed no deadline and
// nothing else bounded a local call), so it fails on the 10s guard below. The
// hang mattered because only this call's return emits instanceKilledMsg, the sole
// clearer of the row's OpKilling fence — so the row sat in `Deleting` forever with
// no error, and D refused to retry it.
func TestKillSessionThroughDaemon_WedgedDaemon_ReturnsActionableError(t *testing.T) {
	sock := wedgedDaemonSocket(t)

	origWith := withDaemonHTTP
	origTimeout := killRPCTimeout
	t.Cleanup(func() { withDaemonHTTP = origWith; killRPCTimeout = origTimeout })

	withDaemonHTTP = func(fn func(*apiclient.Client) error) error {
		return fn(apiclient.NewWithSocket(sock))
	}
	killRPCTimeout = 250 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		done <- killSessionThroughDaemon(daemon.KillSessionRequest{ID: "alpha-id", Title: "alpha", RepoID: "repo-1"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error when the daemon never answers, got nil — the TUI would report a successful kill that never happened")
		}
		// The error must say what happened — a bare "context deadline exceeded" is
		// exactly the unactionable text this fix exists to replace — but it must
		// NOT prescribe a shell command: the recovery is an in-interface restart
		// offer keyed off errDaemonUnresponsive (#2479), so the kill handler needs
		// to recognize the error, and the message must not send the user to a shell.
		if !errors.Is(err, errDaemonUnresponsive) {
			t.Fatalf("error %q must wrap errDaemonUnresponsive so the handler can offer the in-TUI restart", err.Error())
		}
		if strings.Contains(err.Error(), "af daemon restart") || strings.Contains(err.Error(), "af sessions list") {
			t.Fatalf("error %q must not prescribe a shell command; the restart is offered in-interface (#2479)", err.Error())
		}
		// The daemon may still be tearing the session down; claiming the kill
		// failed outright would be a lie the next snapshot contradicts.
		if !strings.Contains(err.Error(), "may still finish") {
			t.Fatalf("error %q must admit the teardown may still complete", err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HUNG: killSessionThroughDaemon never returned against a wedged daemon — this is the reported D-key hang; the row stays in Deleting forever")
	}
}

// TestHTTPCallRetryable_ContextErrorsAreNotRetryable guards the interaction between
// the kill bound and withDaemonHTTP's warm-up retry, which the tests above cannot
// see because they stub withDaemonHTTP.
//
// An expired context surfaces as a TransportError (http.Client reports the
// deadline through the round-trip, which apiclient tags), and TransportError is
// exactly what the warm-up loop retries. Left unguarded, a kill that hit its
// deadline would be re-issued for the whole 5s warm-up window against a context
// that is already dead — every attempt failing instantly — before finally
// reporting the timeout the user should have seen 5 seconds earlier.
func TestHTTPCallRetryable_ContextErrorsAreNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shape http.Client actually produces, as apiclient wraps it.
			wrapped := &apiclient.TransportError{
				Err: &url.Error{Op: "Post", URL: "http://af/v1/KillSession", Err: tc.err},
			}
			if !apiclient.IsTransportError(wrapped) {
				t.Fatal("precondition: a context failure does arrive as a TransportError, which is what the warm-up loop retries")
			}
			if httpCallRetryable(wrapped) {
				t.Fatal("a caller's own deadline/cancel must not be retried as daemon warm-up: every retry re-issues the call with a dead context and fails instantly")
			}
		})
	}

	// The genuine warm-up condition must still retry, or a cold-start kill breaks.
	stillWarming := &apiclient.TransportError{Err: errors.New("dial unix: connect: connection refused")}
	if !httpCallRetryable(stillWarming) {
		t.Fatal("a real transport failure must still be retried while the daemon's socket binds")
	}
}

// TestHTTPCallRetryable_DaemonAdmissionParity pins the cross-transport
// invariant behind the retry classifier. A new daemon admission phase belongs
// in daemon.IsDaemonAdmissionRetryable once; both net/rpc and the TUI's HTTP
// path then inherit it instead of maintaining parallel phase lists.
func TestHTTPCallRetryable_DaemonAdmissionParity(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"startup warming", errDaemonStarting()},
		{"upgrade probation", errors.New("agent-factory daemon is validating an upgrade (transaction tx-2212); retry shortly")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !daemon.IsDaemonAdmissionRetryable(tc.err) {
				t.Fatal("precondition: daemon admission classifier did not recognize the wire error")
			}
			if !httpCallRetryable(tc.err) {
				t.Fatal("TUI HTTP retry drifted from the daemon admission classifier")
			}
		})
	}
}

// TestKillSessionThroughDaemon_DaemonError_SurfacesVerbatim proves the bound did
// not swallow or reshape ordinary failures: a daemon that answers with an error
// still surfaces that error's own text promptly.
func TestKillSessionThroughDaemon_DaemonError_SurfacesVerbatim(t *testing.T) {
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/KillSession", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"error":{"message":"session \"ghost\" not found"}}`))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	origWith := withDaemonHTTP
	t.Cleanup(func() { withDaemonHTTP = origWith })
	withDaemonHTTP = func(fn func(*apiclient.Client) error) error {
		return fn(apiclient.NewWithSocket(sockPath))
	}

	done := make(chan error, 1)
	go func() {
		done <- killSessionThroughDaemon(daemon.KillSessionRequest{ID: "ghost-id", Title: "ghost", RepoID: "repo-1"})
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `session "ghost" not found`) {
			t.Fatalf("want the daemon's own message, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a daemon that answers must not hang the kill")
	}
}
