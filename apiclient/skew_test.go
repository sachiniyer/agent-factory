package apiclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// wedgedServer stands up a Unix-socket HTTP server that ACCEPTS every connection
// and then never answers, releasing only at test cleanup. It reproduces the exact
// shape of the reported hang: the dial succeeds (so the 250ms dialTimeout never
// fires and the error is not a TransportError), and the client is left blocking on
// a read that has no deadline. A daemon wedged in a handler — blocked acquiring a
// session's op lock behind a Lost-recovery — presents identically.
func wedgedServer(t *testing.T) *Client {
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
	return NewWithSocket(sockPath)
}

// TestKillSession_WedgedDaemon_HonorsContext is the anti-hang lock. Against a
// daemon that accepts and never answers, the kill must come back as soon as its
// deadline passes rather than blocking forever.
//
// This is the failure the TUI could not survive: killInstanceCmd's goroutine only
// emits instanceKilledMsg when this call returns, and that message is the ONLY
// thing that clears the row's optimistic OpKilling fence — sync's prune and the
// liveness-observing transitions all skip a row with an in-flight op. So a call
// that never returns strands the row in `Deleting` permanently, with no error
// ever shown and D refusing to retry ("already being deleted").
//
// Before this fix KillSession took no context and the local socket set no
// deadline at any layer (no requestTimeout, no http.Client.Timeout, and only
// ReadHeaderTimeout daemon-side), so this test hung until the go test binary's
// own panic timeout.
func TestKillSession_WedgedDaemon_HonorsContext(t *testing.T) {
	c := wedgedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- c.KillSession(ctx, daemon.KillSessionRequest{Title: "alpha", RepoID: "r"}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from a daemon that never answers, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want a deadline error the caller can map to an actionable message, got %T: %v", err, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("kill took %s to give up; the deadline is not bounding the read", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HUNG: KillSession never returned against a wedged daemon — the row would be stranded in Deleting forever")
	}
}

// TestPreview_VersionSkew_SurfacesActionableError locks the self-diagnosis. A
// daemon older than #1779 has no tab_id field on PreviewRequest and strict-decodes,
// so a newer TUI's preview poll gets `unknown field "tab_id"` — a message that
// names a field the user never typed and offers no remedy.
//
// Preview is the real producer here (apiclient/stream.go sets tab_id on the
// request), which is why the skew shows up on this path and not on kill.
func TestPreview_VersionSkew_SurfacesActionableError(t *testing.T) {
	const daemonMsg = `malformed JSON request body: json: unknown field "tab_id"`
	c := routeServer(t, "Preview", func([]byte) apiproto.Envelope {
		return apiproto.Failure(daemonMsg)
	})

	_, _, _, err := c.Preview(daemon.PreviewRequest{Title: "alpha", TabID: "t-abc"})
	if err == nil {
		t.Fatal("want an error from a skewed daemon")
	}

	var skew *VersionSkewError
	if !errors.As(err, &skew) {
		t.Fatalf("want a VersionSkewError so callers can recognize skew, got %T: %v", err, err)
	}
	if skew.Field != "tab_id" {
		t.Fatalf("want the rejected field named, got %q", skew.Field)
	}
	// The whole point is that the user learns what to DO. Assert the remedy, not
	// just that some error came back.
	for _, want := range []string{"out of date", "af daemon restart", "tab_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q to be actionable", err.Error(), want)
		}
	}
	// The daemon's verbatim text stays available for the log.
	if skew.Detail != daemonMsg {
		t.Fatalf("want the daemon's raw message preserved, got %q", skew.Detail)
	}
}

// TestEnvelopeError_NonSkew_StaysVerbatim guards the blast radius of the skew
// mapping: an ordinary daemon error must pass through byte-identically, since
// callers (and TestControlError_SurfacesEnvelopeMessage) match on that text.
func TestEnvelopeError_NonSkew_StaysVerbatim(t *testing.T) {
	c := routeServer(t, "Preview", func([]byte) apiproto.Envelope {
		return apiproto.Failure(`session "ghost" not found`)
	})
	_, _, _, err := c.Preview(daemon.PreviewRequest{Title: "ghost"})
	if err == nil || err.Error() != `session "ghost" not found` {
		t.Fatalf("want the verbatim daemon message, got %v", err)
	}
	var skew *VersionSkewError
	if errors.As(err, &skew) {
		t.Fatal("an ordinary daemon error must not be misread as a version skew")
	}
}

// TestCall_SendsClientVersionHeader proves every request identifies itself as an
// af client. That header is what lets the daemon tolerate additive fields from a
// newer client while still strict-decoding hand-authored curl requests (#1264).
func TestCall_SendsClientVersionHeader(t *testing.T) {
	got := make(chan string, 1)
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Preview", func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(agentproto.ClientVersionHeader)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PreviewResponse{}))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if _, _, _, err := NewWithSocket(sockPath).Preview(daemon.PreviewRequest{Title: "alpha"}); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	select {
	case v := <-got:
		if v == "" {
			t.Fatal("every af-client request must carry a non-empty client-version header; empty means the daemon strict-decodes it and version skew hard-fails")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}
}

// TestSetClientVersion_StampsHeaderValue proves the stamped version reaches the
// wire, so a daemon-side log can name the client's version.
func TestSetClientVersion_StampsHeaderValue(t *testing.T) {
	prev := clientVersionOrUnknown()
	SetClientVersion("9.9.9")
	t.Cleanup(func() { SetClientVersion(prev) })
	if v := clientVersionOrUnknown(); v != "9.9.9" {
		t.Fatalf("want the stamped version reported, got %q", v)
	}
	// An empty set must not blank out a good value.
	SetClientVersion("")
	if v := clientVersionOrUnknown(); v != "9.9.9" {
		t.Fatalf("an empty SetClientVersion must be a no-op, got %q", v)
	}
}
