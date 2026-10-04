package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

// remoteStatusServer is the intermediary-response seam. Unlike statusServer's
// trusted Unix socket, this client crosses HTTP where a proxy can replace the
// daemon's response and therefore needs positive origin evidence.
func remoteStatusServer(t *testing.T, handle func(r *http.Request) (int, []byte)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := handle(r)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewRemote(srv.URL, "")
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	return c
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
		// default_program does not force the listener posture safe, so the
		// write takes the guarded route — the server registers that twin.
		c := routeServer(t, "SetConfigValueGuarded", func(b []byte) apiproto.Envelope {
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
		// Only unsetting network.listen_addr forces the listener safe; every
		// other unset takes the guarded route, which is what this stub serves.
		c := routeServer(t, "UnsetConfigValueGuarded", func(b []byte) apiproto.Envelope {
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
		c := routeServer(t, "SetConfigValueGuarded", func([]byte) apiproto.Envelope {
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
			env := apiproto.Failure(`unknown route "` + r.URL.Path + `"`)
			env.Error.DaemonRejected = true
			return http.StatusNotFound, mustEnvelope(t, env)
		})
		// Unsetting listen_addr restores the loopback default — the one unset
		// that forces the listener safe, so it stays on the plain route.
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.listen_addr"})
		if !IsRouteNotServed(err) {
			t.Fatalf("a 404 must classify as a missing route, got %T: %v", err, err)
		}
		var missing *RouteNotServedError
		if !errors.As(err, &missing) || missing.Route != "/v1/UnsetConfigValue" {
			t.Fatalf("the error must name the route that 404ed, got %+v", missing)
		}
	})

	t.Run("a legacy remote daemon's unmarked catch-all", func(t *testing.T) {
		c := remoteStatusServer(t, func(r *http.Request) (int, []byte) {
			return http.StatusNotFound, mustEnvelope(t, apiproto.Failure(`unknown route "`+r.URL.Path+`"`))
		})
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.listen_addr"})
		if !IsRouteNotServed(err) {
			t.Fatalf("a legacy daemon catch-all must retain route-skew handling, got %T: %v", err, err)
		}
	})

	// A reverse proxy in front of the daemon can substitute its own 404 after the
	// upstream mutation ran. Without the daemon provenance marker, absence and
	// execution are indistinguishable, so the response must remain uncertain.
	t.Run("a proxy's non-envelope 404", func(t *testing.T) {
		c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
			return http.StatusNotFound, []byte("<html>\n<head><title>404 Not Found</title></head>\n</html>\n")
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
		if err == nil || IsRouteNotServed(err) {
			t.Fatalf("an unmarked 404 must stay uncertain, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "404 Not Found") || !strings.Contains(err.Error(), "outcome could not be confirmed") {
			t.Errorf("the uncertainty must quote who answered, got: %v", err)
		}
	})

	// The proxy shapes that the first version of this MISSED, because it keyed the
	// classification on the body. Each parses as JSON and carries no envelope
	// error, so a check on env.Error never ran (Codex, #3704). The third is the
	// dangerous one: `{"data":null,"error":null}` decodes into a zero-valued
	// response with a NIL error, so a 404 was reported to the caller as SUCCESS.
	for _, body := range []string{
		`{}`,
		`{"message":"not found"}`,
		`{"data":null,"error":null}`,
		`{"error":"not found"}`, // error present but not the envelope's object shape
	} {
		t.Run("a proxy's JSON 404: "+body, func(t *testing.T) {
			c := remoteStatusServer(t, func(*http.Request) (int, []byte) {
				return http.StatusNotFound, []byte(body)
			})
			resp, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"})
			if err == nil || IsRouteNotServed(err) {
				t.Fatalf("an unmarked JSON 404 must stay uncertain, got %T: %v", err, err)
			}
			if resp.Result != nil {
				t.Errorf("a 404 must never yield a decoded result, got %+v", resp.Result)
			}
		})
	}

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

// TestGuardedRouteSelectionIsThe5137CapabilityCheck pins the route-selection
// contract itself: writes that CAN create the refused unauthenticated-network
// posture go to the guarded twin only a refusal-enforcing daemon serves, and
// that daemon's absence answers the write request itself — not a preflight.
func TestGuardedRouteSelectionIsThe5137CapabilityCheck(t *testing.T) {
	// Each case watches which PATH the request lands on; the bodies are
	// identical either way, so the route is the observable proof.
	t.Run("an exposure-capable set takes the guarded route", func(t *testing.T) {
		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.SetConfigValueResponse{
				Result: &config.SetResult{Key: "network.listen_addr", Path: "/remote/config.toml"},
			}))
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "network.listen_addr", Value: "0.0.0.0:8443"})
		if err != nil {
			t.Fatalf("SetConfigValue: %v", err)
		}
		if path != "/v1/SetConfigValueGuarded" {
			t.Errorf("the write must take the guarded route, got %q", path)
		}
	})

	t.Run("a padded boolean still classifies exposure-capable", func(t *testing.T) {
		// The receiving writer canonicalizes before parsing — " false " lands
		// as require_token=false. The client must classify the canonical value:
		// a raw-value ParseBool fails and would route this around the guard.
		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.SetConfigValueResponse{
				Result: &config.SetResult{Key: "network.require_token", Value: "false", Path: "/remote/config.toml"},
			}))
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "network.require_token", Value: " false "})
		if err != nil {
			t.Fatalf("SetConfigValue: %v", err)
		}
		if path != "/v1/SetConfigValueGuarded" {
			t.Errorf("the whitespace-padded false must take the guarded route, got %q", path)
		}
	})

	t.Run("the legacy alias is exposure-capable too", func(t *testing.T) {
		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.SetConfigValueResponse{
				Result: &config.SetResult{Key: "listen_addr", Path: "/remote/config.toml"},
			}))
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "listen_addr", Value: "0.0.0.0:8443"})
		if err != nil {
			t.Fatalf("SetConfigValue: %v", err)
		}
		if path != "/v1/SetConfigValueGuarded" {
			t.Errorf("the wire spelling is the flat alias — it must still take the guarded route, got %q", path)
		}
	})

	t.Run("unsetting require_token takes the guarded route", func(t *testing.T) {
		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.UnsetConfigValueResponse{
				Result: &config.UnsetResult{Key: "network.require_token", Removed: true, Path: "/remote/config.toml"},
			}))
		})
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.require_token"})
		if err != nil {
			t.Fatalf("UnsetConfigValue: %v", err)
		}
		if path != "/v1/UnsetConfigValueGuarded" {
			t.Errorf("the unset must take the guarded route, got %q", path)
		}
	})

	// A daemon that predates #5137 404s the guarded path, and the client must
	// say WHY — a bare "route not served" would read as a missing method, when
	// the truth is the daemon would have served the exposure the write created.
	t.Run("a pre-refusal daemon's 404 becomes the policy refusal", func(t *testing.T) {
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			env := apiproto.Failure(`unknown route "` + r.URL.Path + `"`)
			env.Error.DaemonRejected = true
			return http.StatusNotFound, mustEnvelope(t, env)
		})
		_, err := c.SetConfigValue(daemon.SetConfigValueRequest{Key: "network.listen_addr", Value: "0.0.0.0:8443"})
		if err == nil {
			t.Fatal("the guarded route's 404 must fail the write")
		}
		if IsRouteNotServed(err) {
			t.Errorf("the guarded 404 is translated out of the generic skew class, got %T: %v", err, err)
		}
		for _, want := range []string{"predates af's unauthenticated-listener refusal", "network.listen_addr", "nothing was written"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the translated refusal must contain %q, got: %v", want, err)
			}
		}
	})

	t.Run("the unset direction translates the same way", func(t *testing.T) {
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			env := apiproto.Failure(`unknown route "` + r.URL.Path + `"`)
			env.Error.DaemonRejected = true
			return http.StatusNotFound, mustEnvelope(t, env)
		})
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.require_token"})
		if err == nil || !strings.Contains(err.Error(), "predates af's unauthenticated-listener refusal") {
			t.Fatalf("want the translated policy refusal, got: %v", err)
		}
	})

	// Only writes that force the listener posture safe BY THEMSELVES keep the
	// plain routes — token ON, loopback or empty listen_addr. Every other write
	// goes guarded: an old daemon's write handler ends in a whole-file
	// ApplyConfig, so an unrelated key on a file already holding the refused
	// posture binds it just the same (#5137 review).
	t.Run("safe writes keep the plain routes", func(t *testing.T) {
		for _, tc := range []struct {
			req  daemon.SetConfigValueRequest
			want string
		}{
			{daemon.SetConfigValueRequest{Key: "network.require_token", Value: "true"}, "/v1/SetConfigValue"},
			{daemon.SetConfigValueRequest{Key: "network.listen_addr", Value: "127.0.0.1:8443"}, "/v1/SetConfigValue"},
			{daemon.SetConfigValueRequest{Key: "network.listen_addr", Value: ""}, "/v1/SetConfigValue"},
		} {
			var path string
			c := statusServer(t, func(r *http.Request) (int, []byte) {
				path = r.URL.Path
				return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.SetConfigValueResponse{
					Result: &config.SetResult{Key: tc.req.Key, Value: tc.req.Value, Path: "/remote/config.toml"},
				}))
			})
			if _, err := c.SetConfigValue(tc.req); err != nil {
				t.Fatalf("SetConfigValue(%+v): %v", tc.req, err)
			}
			if path != tc.want {
				t.Errorf("SetConfigValue(%s=%s) took %q, want %q", tc.req.Key, tc.req.Value, path, tc.want)
			}
		}

		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.UnsetConfigValueResponse{
				Result: &config.UnsetResult{Key: "network.listen_addr", Removed: true, Path: "/remote/config.toml"},
			}))
		})
		if _, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.listen_addr"}); err != nil {
			t.Fatalf("UnsetConfigValue: %v", err)
		}
		if path != "/v1/UnsetConfigValue" {
			t.Errorf("unsetting listen_addr disables the listener — plain route, got %q", path)
		}
	})

	// The corollary: an unrelated write is the case the whole-file apply makes
	// dangerous — the file's own posture decides what binds, so the write must
	// take the guarded route and fail closed on a pre-#5137 daemon.
	t.Run("unrelated writes take the guarded route", func(t *testing.T) {
		for _, tc := range []struct {
			req  daemon.SetConfigValueRequest
			want string
		}{
			{daemon.SetConfigValueRequest{Key: "default_program", Value: "codex"}, "/v1/SetConfigValueGuarded"},
			{daemon.SetConfigValueRequest{Key: "network.allow_unauthenticated_network", Value: "true"}, "/v1/SetConfigValueGuarded"},
			{daemon.SetConfigValueRequest{Key: "network.allow_unauthenticated_network", Value: "false"}, "/v1/SetConfigValueGuarded"},
			{daemon.SetConfigValueRequest{Key: "network.require_token", Value: "not-a-bool"}, "/v1/SetConfigValueGuarded"},
		} {
			var path string
			c := statusServer(t, func(r *http.Request) (int, []byte) {
				path = r.URL.Path
				return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.SetConfigValueResponse{
					Result: &config.SetResult{Key: tc.req.Key, Value: tc.req.Value, Path: "/remote/config.toml"},
				}))
			})
			if _, err := c.SetConfigValue(tc.req); err != nil {
				t.Fatalf("SetConfigValue(%+v): %v", tc.req, err)
			}
			if path != tc.want {
				t.Errorf("SetConfigValue(%s=%s) took %q, want %q", tc.req.Key, tc.req.Value, path, tc.want)
			}
		}

		var path string
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			path = r.URL.Path
			return http.StatusOK, mustEnvelope(t, apiproto.Success(daemon.UnsetConfigValueResponse{
				Result: &config.UnsetResult{Key: "network.preview_listen_addr", Removed: true, Path: "/remote/config.toml"},
			}))
		})
		if _, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.preview_listen_addr"}); err != nil {
			t.Fatalf("UnsetConfigValue: %v", err)
		}
		if path != "/v1/UnsetConfigValueGuarded" {
			t.Errorf("an unset that does not force the control listener safe takes the guarded route, got %q", path)
		}
	})

	// And a plain-route 404 stays the generic skew error — only the guarded
	// 404 carries the policy translation.
	t.Run("a plain-route 404 stays the generic missing-route error", func(t *testing.T) {
		c := statusServer(t, func(r *http.Request) (int, []byte) {
			env := apiproto.Failure(`unknown route "` + r.URL.Path + `"`)
			env.Error.DaemonRejected = true
			return http.StatusNotFound, mustEnvelope(t, env)
		})
		_, err := c.UnsetConfigValue(daemon.UnsetConfigValueRequest{Key: "network.listen_addr"})
		if !IsRouteNotServed(err) {
			t.Fatalf("a plain-route 404 must classify as a missing route, got %T: %v", err, err)
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
