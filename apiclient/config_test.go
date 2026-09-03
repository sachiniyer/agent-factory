package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The config write pair and the two things a caller must be able to tell apart
// on the way back: an answer from the daemon, and no route to answer with
// (#3679). `af config set --daemon-url` routes here, and its only alternative to
// a clear refusal would be writing the CALLER's config file for a change meant
// for another machine — so "the daemon does not serve this" cannot arrive as an
// ordinary error.

// statusServer is routeServer's sibling for cases that need control of the HTTP
// STATUS as well as the body — the 404 branch is a status-keyed inference, so a
// stub that only chose the body could not exercise it. handle returns the status
// and the raw bytes to write, verbatim, so a case can also answer with something
// that is not an envelope at all.
func statusServer(t *testing.T, handle func(r *http.Request) (int, []byte)) *Client {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status, body := handle(r)
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewWithSocket(sockPath)
}

func mustEnvelope(t *testing.T, env apiproto.Envelope) []byte {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func TestConfigWriteRoundTrips(t *testing.T) {
	t.Run("SetConfigValue", func(t *testing.T) {
		var got daemon.SetConfigValueRequest
		c := routeServer(t, "SetConfigValue", func(b []byte) apiproto.Envelope {
			_ = json.Unmarshal(b, &got)
			return apiproto.Success(daemon.SetConfigValueResponse{
				Result:        &config.SetResult{Key: got.Key, Value: got.Value, Path: "/remote/config.toml"},
				RestartNotice: "applied to the running daemon",
			})
		})
		resp, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
		if err != nil {
			t.Fatalf("SetConfigValue: %v", err)
		}
		if got.Key != "default_program" || got.Value != "codex" {
			t.Fatalf("daemon saw %+v, want key=default_program value=codex", got)
		}
		if resp.Result == nil || resp.Result.Path != "/remote/config.toml" {
			t.Fatalf("decoded result = %+v, want the daemon's own path", resp.Result)
		}
		if resp.RestartNotice != "applied to the running daemon" {
			t.Errorf("the daemon's effect notice must survive the round trip, got %q", resp.RestartNotice)
		}
	})

	t.Run("UnsetConfigValue", func(t *testing.T) {
		var got daemon.UnsetConfigValueRequest
		c := routeServer(t, "UnsetConfigValue", func(b []byte) apiproto.Envelope {
			_ = json.Unmarshal(b, &got)
			return apiproto.Success(daemon.UnsetConfigValueResponse{
				Result: &config.UnsetResult{Key: got.Key, Removed: true, Path: "/remote/config.toml"},
			})
		})
		resp, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "ssh.host_key_verification"})
		if err != nil {
			t.Fatalf("UnsetConfigValue: %v", err)
		}
		if got.Key != "ssh.host_key_verification" {
			t.Fatalf("daemon saw key %q", got.Key)
		}
		if resp.Result == nil || !resp.Result.Removed {
			t.Fatalf("decoded result = %+v, want Removed", resp.Result)
		}
	})

	// An envelope ERROR is the daemon's considered answer — an admission refusal,
	// an unknown key — and must stay an ordinary error. Misreading one as a
	// missing route would tell the caller "nothing happened, upgrade the daemon"
	// about a daemon that ran the handler and said no.
	t.Run("an envelope error is not a missing route", func(t *testing.T) {
		c := routeServer(t, "SetConfigValue", func([]byte) apiproto.Envelope {
			return apiproto.Failure("agent-factory daemon is handing off to an upgrade; retry shortly")
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
		if err == nil {
			t.Fatal("a refusing daemon must surface as an error")
		}
		if IsRouteNotServed(err) {
			t.Errorf("a handler that ran and refused is not a missing route: %v", err)
		}
		if !strings.Contains(err.Error(), "handing off to an upgrade") {
			t.Errorf("the daemon's own message must survive verbatim, got: %v", err)
		}
	})
}

func TestRouteNotServedIsDistinguishable(t *testing.T) {
	// The daemon's own catch-all: 404 carrying the envelope
	// (daemon/httpserver.go). rpcHandler answers only 200/400/405/413/500/503, so
	// a 404 on a /v1 route can come from nowhere else.
	t.Run("the daemon's 404 envelope", func(t *testing.T) {
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			return http.StatusNotFound, mustEnvelope(t, apiproto.Failure(`unknown route "`+r.URL.Path+`"`))
		})
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "sandbox.ssh"})
		if !IsRouteNotServed(err) {
			t.Fatalf("a 404 must classify as a missing route, got %T: %v", err, err)
		}
		var missing *RouteNotServedError
		if !errors.As(err, &missing) || missing.Route != "/v1/UnsetConfigValue" {
			t.Fatalf("the error must name the route that 404ed, got %+v", missing)
		}
	})

	// A reverse proxy in front of the daemon — the deployment docs/remote-http-auth.md
	// recommends, since af terminates no TLS — answers with its OWN 404 page, which
	// is not an envelope. Reporting that as "malformed response envelope" would
	// bury the only fact the caller can act on.
	t.Run("a proxy's non-envelope 404", func(t *testing.T) {
		c := statusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusNotFound, []byte("<html>\n<head><title>404 Not Found</title></head>\n</html>\n")
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
		if !IsRouteNotServed(err) {
			t.Fatalf("a non-envelope 404 must still classify as a missing route, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "404 Not Found") {
			t.Errorf("the refusal must quote who answered, got: %v", err)
		}
	})

	// Every other status keeps its existing meaning. A 500 is the daemon
	// answering, so a caller must not read it as "nothing happened".
	t.Run("a non-404 malformed body is still a malformed envelope", func(t *testing.T) {
		c := statusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusInternalServerError, []byte("not json")
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
		if err == nil || IsRouteNotServed(err) {
			t.Fatalf("a 500 must not classify as a missing route, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "malformed response envelope") {
			t.Errorf("want the existing malformed-envelope error, got: %v", err)
		}
	})
}

func TestHealthReportsTheDaemonVersion(t *testing.T) {
	t.Run("version and method", func(t *testing.T) {
		var method, path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			method, path = r.Method, r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.PingResponse{OK: true, Version: "1.9.0"}))
		})
		resp, err := c.Health(context.Background())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if method != http.MethodGet || path != "/v1/health" {
			t.Errorf("Health must be GET /v1/health, got %s %s", method, path)
		}
		if !resp.OK || resp.Version != "1.9.0" {
			t.Errorf("decoded ping = %+v, want OK with version 1.9.0", resp)
		}
	})

	// A daemon predating #1044 answers Ping with no version. Empty from a
	// RESPONDING daemon is a positive skew signal, so it must arrive as a
	// successful read of an empty field rather than as an error.
	t.Run("a daemon that reports no version still answers", func(t *testing.T) {
		c := statusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.PingResponse{OK: true}))
		})
		resp, err := c.Health(context.Background())
		if err != nil || !resp.OK || resp.Version != "" {
			t.Fatalf("Health = %+v, %v; want a successful read with an empty version", resp, err)
		}
	})
}

// TestBodySnippetCutsOnARuneBoundary covers the non-ASCII proxy error page. The
// limit is a byte cap, so a naive slice splits whatever rune it lands inside and
// ends the operator's error message in a U+FFFD fragment.
//
// It runs every multi-byte width on purpose, and that is not thoroughness for
// its own sake: whether the byte limit lands mid-rune depends on the limit
// MODULO the rune width, so a single width can pass while the bug is fully
// present. At today's limit of 200 the 2- and 4-byte cases land exactly on a
// boundary by luck and prove nothing; the 3-byte case is the one that bites.
// Fixing the width to whichever one happens to bite today would silently stop
// testing anything the moment the limit changed.
func TestBodySnippetCutsOnARuneBoundary(t *testing.T) {
	for _, r := range []struct {
		name string
		char string
	}{
		{"2-byte", "\u00e9"},     // é
		{"3-byte", "\u3042"},     // あ
		{"4-byte", "\U0001f600"}, // 😀
	} {
		t.Run(r.name, func(t *testing.T) {
			// Long enough that the limit falls well inside the body whatever the width.
			body := []byte(strings.Repeat(r.char, bodySnippetLimit))
			got := bodySnippet(body)
			if !utf8.ValidString(got) {
				t.Fatalf("the snippet must stay valid UTF-8, got %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("a rune-split snippet renders as U+FFFD, got %q", got)
			}
			if !strings.HasSuffix(got, "\u2026") {
				t.Errorf("a truncated snippet must be marked as truncated, got %q", got)
			}
			if len(got) > bodySnippetLimit+len("\u2026") {
				t.Errorf("the byte cap must still hold, got %d bytes", len(got))
			}
		})
	}

	// Short bodies pass through whole, whitespace-collapsed.
	if snippet := bodySnippet([]byte("  404   Not Found\n")); snippet != "404 Not Found" {
		t.Errorf("a short body must be whitespace-collapsed and kept whole, got %q", snippet)
	}
	if snippet := bodySnippet(nil); snippet != "empty response body" {
		t.Errorf("an empty body must say so, got %q", snippet)
	}
}
