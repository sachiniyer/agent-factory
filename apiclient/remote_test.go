package apiclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// TestParseDaemonURL_AcceptsHTTPSchemesRejectsTLS proves the URL parser derives
// the http/ws authorities from either plaintext scheme and refuses a TLS scheme
// (with the actionable HTTP-only error) or malformed input — the whole point
// being that a stale wss:// config from the old pinned-TLS listener fails loudly
// rather than mysteriously.
func TestParseDaemonURL_AcceptsHTTPSchemesRejectsTLS(t *testing.T) {
	ok := []struct {
		in, http, ws string
	}{
		{"http://host:8443", "http://host:8443", "ws://host:8443"},
		{"ws://host:8443", "http://host:8443", "ws://host:8443"},
		{"http://127.0.0.1:9000/ignored/path", "http://127.0.0.1:9000", "ws://127.0.0.1:9000"},
		{"HTTP://Host:1", "http://Host:1", "ws://Host:1"},
	}
	for _, tc := range ok {
		gotHTTP, gotWS, err := parseDaemonURL(tc.in)
		if err != nil {
			t.Fatalf("parseDaemonURL(%q): unexpected error %v", tc.in, err)
		}
		if gotHTTP != tc.http || gotWS != tc.ws {
			t.Fatalf("parseDaemonURL(%q) = (%q,%q), want (%q,%q)", tc.in, gotHTTP, gotWS, tc.http, tc.ws)
		}
	}
	// A TLS scheme is rejected with the HTTP-only migration message.
	for _, in := range []string{"wss://host:8443", "https://host:8443"} {
		_, _, err := parseDaemonURL(in)
		if err == nil {
			t.Fatalf("parseDaemonURL(%q): want error, got nil", in)
		}
		if !strings.Contains(err.Error(), "HTTP-only") {
			t.Fatalf("parseDaemonURL(%q): want HTTP-only guidance, got %v", in, err)
		}
	}
	bad := []string{"host:8443", "http://", ""}
	for _, in := range bad {
		if _, _, err := parseDaemonURL(in); err == nil {
			t.Fatalf("parseDaemonURL(%q): want error, got nil", in)
		}
	}
}

// TestParseDaemonURL_RejectsEmptyHost pins the #1784 contract: a URL carrying a
// scheme but no host is rejected at VALIDATION with the actionable "missing host"
// message, not admitted and left to fail later as `dial tcp :8443: connect:
// connection refused`. The `:8443` / `:` forms are the regression that matters —
// net/url gives them a non-empty Host, so the old u.Host check waved them
// through; they are exactly what `http://${DAEMON_HOST}:8443` expands to when the
// variable is unset.
func TestParseDaemonURL_RejectsEmptyHost(t *testing.T) {
	for _, in := range []string{"http://:8443", "ws://:8443", "http://:", "http:///path", "ws://", "http://"} {
		_, _, err := parseDaemonURL(in)
		if err == nil {
			t.Fatalf("parseDaemonURL(%q): want error, got nil", in)
		}
		if !strings.Contains(err.Error(), "missing host") {
			t.Fatalf("parseDaemonURL(%q): want actionable missing-host error, got %v", in, err)
		}
	}
	// A real host — including the IPv6 literal whose brackets Hostname() strips —
	// still parses, so the check narrows nothing it shouldn't.
	for _, in := range []string{"http://host:8443", "http://[::1]:8443"} {
		if _, _, err := parseDaemonURL(in); err != nil {
			t.Fatalf("parseDaemonURL(%q): unexpected error %v", in, err)
		}
	}
}

// clearTargetEnv unsets every AF_DAEMON_* env var (and any bound flag) so a test's
// remote-target resolution starts from a known-empty state, then restores the flag
// vars on cleanup. Env vars are cleared via t.Setenv, which auto-restores.
func clearTargetEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envDaemonURL, "")
	t.Setenv(envDaemonToken, "")
	ou, ot := FlagDaemonURL, FlagDaemonToken
	FlagDaemonURL, FlagDaemonToken = "", ""
	t.Cleanup(func() { FlagDaemonURL, FlagDaemonToken = ou, ot })
}

// TestResolveTarget_FlagBeatsEnvUnsetIsLocal proves the precedence contract:
// unset ⇒ local (empty URL), env is the fallback, and a flag overrides env.
func TestResolveTarget_FlagBeatsEnvUnsetIsLocal(t *testing.T) {
	clearTargetEnv(t)
	if IsRemoteTarget() {
		t.Fatal("unset target must be local")
	}

	t.Setenv(envDaemonURL, "http://env-host:1")
	t.Setenv(envDaemonToken, "env-tok")
	if url, tok := resolveTarget(); url != "http://env-host:1" || tok != "env-tok" {
		t.Fatalf("env fallback lost: got (%q,%q)", url, tok)
	}
	if !IsRemoteTarget() {
		t.Fatal("env-set target must be remote")
	}

	FlagDaemonURL, FlagDaemonToken = "http://flag-host:2", "flag-tok"
	if url, tok := resolveTarget(); url != "http://flag-host:2" || tok != "flag-tok" {
		t.Fatalf("flag must beat env: got (%q,%q)", url, tok)
	}
}

// httpSnapshotServer stands up a REAL plain-HTTP server answering POST
// /v1/Snapshot exactly as the daemon does (shared apiproto.WriteEnvelope), but
// GATED on the bearer token like the PR3 TCP listener: a request whose token
// (header OR ?access_token=) mismatches wantToken gets a 401 failure envelope,
// never the data.
func httpSnapshotServer(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if agentproto.TokenFromRequest(r) != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = apiproto.WriteEnvelope(w, apiproto.Failure("unauthorized"))
			return
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{Instances: richInstances()}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestNewRemote_RESTRoundTripWithToken is the core remote proof: a Client built
// with NewRemote dials a real plain-HTTP server, threads the bearer token, and
// round-trips the snapshot byte-identically — the network twin of the local
// unix-socket parity test.
func TestNewRemote_RESTRoundTripWithToken(t *testing.T) {
	srv := httpSnapshotServer(t, "secret-token")

	c, err := NewRemote(srv.URL, "secret-token")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	got, err := c.Snapshot(daemon.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot over HTTP: %v", err)
	}
	if len(got) != len(richInstances()) || got[0].Title != "alpha" {
		t.Fatalf("remote snapshot decoded wrong: %+v", got)
	}
}

// TestNewRemote_WrongToken401 proves a bad/missing token surfaces the daemon's
// unauthorized message as a clean Go error (not a crash, not a silent empty) —
// the failure mode the operator sees when the token is wrong.
func TestNewRemote_WrongToken401(t *testing.T) {
	srv := httpSnapshotServer(t, "secret-token")

	for _, badTok := range []string{"", "wrong-token"} {
		c, err := NewRemote(srv.URL, badTok)
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		_, err = c.Snapshot(daemon.SnapshotRequest{})
		if err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("token %q: want unauthorized error, got %v", badTok, err)
		}
	}
}

// TestNewRemote_RejectsTLSURL proves NewRemote refuses a wss:///https:// target
// up front with the HTTP-only guidance, so a stale TLS config never silently
// half-dials.
func TestNewRemote_RejectsTLSURL(t *testing.T) {
	for _, in := range []string{"wss://host:8443", "https://host:8443"} {
		if _, err := NewRemote(in, "tok"); err == nil {
			t.Fatalf("NewRemote(%q): want HTTP-only error, got nil", in)
		}
	}
}

// stallingListener accepts TCP connections but NEVER sends an HTTP response — it
// holds every accepted conn open and idle. A client dialing it gets a completed
// TCP connect and then blocks waiting for response headers that never come: the
// plaintext analogue of the half-open condition that hung every remote call
// before #1730. It returns the listen address; the listener and its held conns
// close on cleanup.
func stallingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conns := make(chan net.Conn, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			conns <- conn // hold it open, never read/write — stall the response
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// withShrunkRemoteTimeouts temporarily shrinks the remote round-trip timeouts so a
// stall test proves the bound FIRES without waiting the full multi-second budget,
// restoring them on cleanup. It exercises the real NewRemote/DialStream wiring
// (which reads these vars) rather than a hand-rolled client.
func withShrunkRemoteTimeouts(t *testing.T) {
	t.Helper()
	od, oh, or := remoteDialTimeout, remoteWSHandshakeTimeout, remoteRequestTimeout
	remoteDialTimeout = 500 * time.Millisecond
	remoteWSHandshakeTimeout = 500 * time.Millisecond
	remoteRequestTimeout = 2 * time.Second
	t.Cleanup(func() {
		remoteDialTimeout, remoteWSHandshakeTimeout, remoteRequestTimeout = od, oh, or
	})
}

// TestNewRemote_NeverRespondsAfterConnectTimesOut is the #1730 regression for the
// REST path: a remote daemon that accepts the TCP connection but never sends a
// response must make a REST call return a timeout error within the configured
// budget, not hang forever. The single overall request deadline bounds it.
func TestNewRemote_NeverRespondsAfterConnectTimesOut(t *testing.T) {
	withShrunkRemoteTimeouts(t)
	addr := stallingListener(t)

	c, err := NewRemote("http://"+addr, "tok")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	errc := make(chan error, 1)
	go func() {
		_, e := c.Snapshot(daemon.SnapshotRequest{})
		errc <- e
	}()
	select {
	case e := <-errc:
		if e == nil {
			t.Fatal("stalled response: want a timeout error, got nil")
		}
		if !strings.Contains(e.Error(), "timeout") && !strings.Contains(e.Error(), "deadline exceeded") {
			t.Fatalf("stalled response: want a timeout error, got %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: Snapshot did not return on a stalled response (#1730 regression)")
	}
}

// TestDialStream_StalledHandshakeTimesOut proves the WS path is bounded too: a
// server that accepts TCP but never answers the upgrade makes DialStream error
// out (via the WS-handshake timeout) instead of hanging, even though the attach
// call site passes context.Background() (no caller-side deadline). Plain HTTP has
// no TLS-handshake timeout, so this bound is what preserves the #1730 protection.
func TestDialStream_StalledHandshakeTimesOut(t *testing.T) {
	withShrunkRemoteTimeouts(t)
	addr := stallingListener(t)

	c, err := NewRemote("http://"+addr, "tok")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	errc := make(chan error, 1)
	go func() {
		_, e := c.DialStream(context.Background(), "alpha", "", 0, 0)
		errc <- e
	}()
	select {
	case e := <-errc:
		if e == nil {
			t.Fatal("stalled WS handshake: want an error, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HANG: DialStream did not return on a stalled handshake (#1730 regression)")
	}
}

// TestNewRemote_SlowButProgressingResponseNotKilled is the Greptile correctness
// guard for a slow SYNCHRONOUS create: a valid RPC whose server does real work for
// a while and answers JUST UNDER the overall deadline (like a remote docker/ssh
// CreateSession provisioning) must SUCCEED, not be severed early. It proves the
// single deadline is a wedged-connection backstop, not a race against slow work.
func TestNewRemote_SlowButProgressingResponseNotKilled(t *testing.T) {
	withShrunkRemoteTimeouts(t) // remoteRequestTimeout is now 2s
	// The daemon takes a substantial slice of the deadline before responding — well
	// past what a snappy read would need — yet lands comfortably before it. Sized
	// off the live var so it stays under the deadline if the value ever changes.
	serverWork := remoteRequestTimeout / 2
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(serverWork):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SnapshotResponse{Instances: richInstances()}))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewRemote(srv.URL, "tok")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	got, err := c.Snapshot(daemon.SnapshotRequest{})
	if err != nil {
		t.Fatalf("slow-but-progressing response must NOT be timed out, got %v", err)
	}
	if len(got) != len(richInstances()) || got[0].Title != "alpha" {
		t.Fatalf("slow response decoded wrong: %+v", got)
	}
}

// TestDialStream_RemoteThreadsTokenHeaderAndQuery proves the WS handshake to a
// REMOTE daemon carries the token BOTH as an Authorization header (the Go client)
// and as ?access_token= (browser parity), over plain HTTP.
func TestDialStream_RemoteThreadsTokenHeaderAndQuery(t *testing.T) {
	var gotHeader, gotQueryTok string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		gotHeader = agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader))
		gotQueryTok = r.URL.Query().Get(agentproto.AccessTokenQueryParam)
		conn, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if aerr != nil {
			return
		}
		_ = agentproto.WriteFrame(context.Background(), conn, agentproto.PTYOutFrame([]byte("ok")))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewRemote(srv.URL, "secret-token")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := c.DialStream(ctx, "alpha", "", 0, 0)
	if err != nil {
		t.Fatalf("DialStream over HTTP: %v", err)
	}
	defer func() { _ = sc.Conn.Close(websocket.StatusNormalClosure, "") }()

	if gotHeader != "secret-token" {
		t.Fatalf("Authorization header token = %q, want secret-token", gotHeader)
	}
	if gotQueryTok != "secret-token" {
		t.Fatalf("?access_token= = %q, want secret-token", gotQueryTok)
	}
}
