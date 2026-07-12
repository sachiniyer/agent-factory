package apiclient

import (
	"context"
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

// TestParseDaemonURL_AcceptsTLSSchemesRejectsPlaintext proves the URL parser
// derives the https/wss authorities from either TLS scheme and refuses plaintext
// or malformed input — the whole point being that a token never rides clear-text.
func TestParseDaemonURL_AcceptsTLSSchemesRejectsPlaintext(t *testing.T) {
	ok := []struct {
		in, http, ws string
	}{
		{"wss://host:8443", "https://host:8443", "wss://host:8443"},
		{"https://host:8443", "https://host:8443", "wss://host:8443"},
		{"wss://127.0.0.1:9000/ignored/path", "https://127.0.0.1:9000", "wss://127.0.0.1:9000"},
		{"HTTPS://Host:1", "https://Host:1", "wss://Host:1"},
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
	bad := []string{"ws://host:8443", "http://host:8443", "host:8443", "wss://", ""}
	for _, in := range bad {
		if _, _, err := parseDaemonURL(in); err == nil {
			t.Fatalf("parseDaemonURL(%q): want error, got nil", in)
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
	t.Setenv(envTLSFingerprint, "")
	ou, ot, of := FlagDaemonURL, FlagDaemonToken, FlagTLSFingerprint
	FlagDaemonURL, FlagDaemonToken, FlagTLSFingerprint = "", "", ""
	t.Cleanup(func() { FlagDaemonURL, FlagDaemonToken, FlagTLSFingerprint = ou, ot, of })
}

// TestResolveTarget_FlagBeatsEnvUnsetIsLocal proves the precedence contract:
// unset ⇒ local (empty URL), env is the fallback, and a flag overrides env.
func TestResolveTarget_FlagBeatsEnvUnsetIsLocal(t *testing.T) {
	clearTargetEnv(t)
	if IsRemoteTarget() {
		t.Fatal("unset target must be local")
	}

	t.Setenv(envDaemonURL, "wss://env-host:1")
	t.Setenv(envDaemonToken, "env-tok")
	if url, tok, _ := resolveTarget(); url != "wss://env-host:1" || tok != "env-tok" {
		t.Fatalf("env fallback lost: got (%q,%q)", url, tok)
	}
	if !IsRemoteTarget() {
		t.Fatal("env-set target must be remote")
	}

	FlagDaemonURL, FlagDaemonToken = "wss://flag-host:2", "flag-tok"
	if url, tok, _ := resolveTarget(); url != "wss://flag-host:2" || tok != "flag-tok" {
		t.Fatalf("flag must beat env: got (%q,%q)", url, tok)
	}
}

// tlsSnapshotServer stands up a REAL TLS HTTP server answering POST /v1/Snapshot
// exactly as the daemon does (shared apiproto.WriteEnvelope), but GATED on the
// bearer token like the PR3 TCP listener: a request whose token (header OR
// ?access_token=) mismatches wantToken gets a 401 failure envelope, never the
// data. It returns the server plus its cert's pinned fingerprint.
func tlsSnapshotServer(t *testing.T, wantToken string) (*httptest.Server, string) {
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
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, agentproto.CertFingerprint(srv.Certificate().Raw)
}

// TestNewRemote_RESTRoundTripWithPinAndToken is the core remote proof: a Client
// built with NewRemote dials a real TLS server, pins its self-signed cert by
// fingerprint (no InsecureSkipVerify escape), threads the bearer token, and
// round-trips the snapshot byte-identically — the network twin of the local
// unix-socket parity test.
func TestNewRemote_RESTRoundTripWithPinAndToken(t *testing.T) {
	srv, pin := tlsSnapshotServer(t, "secret-token")

	c, err := NewRemote(srv.URL, "secret-token", pin)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	got, err := c.Snapshot(daemon.SnapshotRequest{})
	if err != nil {
		t.Fatalf("Snapshot over TLS: %v", err)
	}
	if len(got) != len(richInstances()) || got[0].Title != "alpha" {
		t.Fatalf("remote snapshot decoded wrong: %+v", got)
	}
}

// TestNewRemote_WrongToken401 proves a bad/missing token surfaces the daemon's
// unauthorized message as a clean Go error (not a crash, not a silent empty) —
// the failure mode the operator sees when the token is wrong.
func TestNewRemote_WrongToken401(t *testing.T) {
	srv, pin := tlsSnapshotServer(t, "secret-token")

	for _, badTok := range []string{"", "wrong-token"} {
		c, err := NewRemote(srv.URL, badTok, pin)
		if err != nil {
			t.Fatalf("NewRemote: %v", err)
		}
		_, err = c.Snapshot(daemon.SnapshotRequest{})
		if err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("token %q: want unauthorized error, got %v", badTok, err)
		}
	}
}

// TestNewRemote_FingerprintMismatchRefused proves a substituted cert is refused:
// pinning the WRONG fingerprint aborts the TLS handshake with an actionable
// mismatch message — verification is real, not skipped.
func TestNewRemote_FingerprintMismatchRefused(t *testing.T) {
	srv, _ := tlsSnapshotServer(t, "secret-token")
	wrongPin := "sha256:" + strings.Repeat("ab", 32)

	c, err := NewRemote(srv.URL, "secret-token", wrongPin)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	_, err = c.Snapshot(daemon.SnapshotRequest{})
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("want fingerprint mismatch refusal, got %v", err)
	}
}

// TestNewRemote_NoPinRejectsSelfSigned proves that WITHOUT a pin the client falls
// to system-root verification — which a self-signed daemon cert fails — rather
// than silently trusting it. This is the guard that we never InsecureSkipVerify.
func TestNewRemote_NoPinRejectsSelfSigned(t *testing.T) {
	srv, _ := tlsSnapshotServer(t, "secret-token")

	c, err := NewRemote(srv.URL, "secret-token", "")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	if _, err := c.Snapshot(daemon.SnapshotRequest{}); err == nil {
		t.Fatal("want TLS verification failure against an un-pinned self-signed cert, got nil")
	}
}

// TestDialStream_RemoteThreadsTokenHeaderAndQuery proves the WS handshake to a
// REMOTE daemon carries the token BOTH as an Authorization header (the Go client)
// and as ?access_token= (browser parity), over TLS with the cert pinned.
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
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	pin := agentproto.CertFingerprint(srv.Certificate().Raw)

	c, err := NewRemote(srv.URL, "secret-token", pin)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := c.DialStream(ctx, "alpha", "", 0, 0)
	if err != nil {
		t.Fatalf("DialStream over TLS: %v", err)
	}
	defer func() { _ = sc.Conn.Close(websocket.StatusNormalClosure, "") }()

	if gotHeader != "secret-token" {
		t.Fatalf("Authorization header token = %q, want secret-token", gotHeader)
	}
	if gotQueryTok != "secret-token" {
		t.Fatalf("?access_token= = %q, want secret-token", gotQueryTok)
	}
}
