package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The two properties #3651 requires as TESTS rather than statements, because a
// pprof profile is a dump of live daemon memory (session titles, worktree paths,
// prompt text):
//
//  1. the handler is unreachable on the network listener EVEN WHEN ENABLED;
//  2. the default-off state answers NOT FOUND, not an empty body.
//
// Both are driven end to end through startHTTPServer — the real bind path that
// wires the unix socket and the TCP listener from one config — rather than
// against a hand-assembled handler. That matters: both listeners serve ONE mux
// (httpserver.go), and the whole enforcement of property 1 is which of the two
// handler chains the pprof wrapper is spliced into. A test that built its own
// chain would be checking its own wiring, not the daemon's.

const pprofPath = debugPprofPrefix + "goroutine?debug=1"

// unixSocketClient dials the daemon's HTTP unix socket. The host in the URL is
// ignored by the dialer and exists only to make a well-formed request.
func unixSocketClient(t *testing.T) *http.Client {
	t.Helper()
	sockPath, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// readBody returns the status and the WHOLE body. The body is the point of
// property 2 — "not-found, not an empty body" is a claim about bytes — so no
// helper here is allowed to discard it.
func readBody(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestDebugPprof_EnabledIsUnixOnly is invariant 1. With the key ON, the profile
// is served over the unix control socket and is NOT reachable on the daemon's TCP
// listener — while that listener is demonstrably up, and while the request
// carries a VALID bearer token. The token half is what makes this a statement
// about the route table rather than about the auth gate: a 404 that a missing
// credential could explain would prove nothing.
func TestDebugPprof_EnabledIsUnixOnly(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	cfg.DebugPprof = true
	cfg.ListenAddr = "127.0.0.1:0" // :0 so this never races the real 8443
	// Enforce the token for EVERY peer, loopback included. Without
	// require_loopback_token the listener exempts 127.0.0.1 on a loopback bind, and
	// the tokenless probe at the end of this test would then pass for the wrong
	// reason — it would be reaching the mux, not being refused by the gate.
	cfg.RequireToken = true
	cfg.RequireLoopbackToken = true
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	// Over the unix socket: a real goroutine profile, not the index page and not
	// an empty 200. This is the anti-vacuous half — without it, a build that
	// served pprof nowhere at all would pass the network assertion below.
	status, body := readBody(t, mustGet(t, unixSocketClient(t), "http://af"+pprofPath, ""))
	require.Equal(t, http.StatusOK, status, "the unix control socket must serve the profile when the key is on")
	assert.Contains(t, body, "goroutine profile: total",
		"the socket must return the NAMED profile; net/http/pprof resolves the profile name by cutting its own %q prefix, so a mount that does not strip /v1 silently serves the index page instead", stdlibPprofPrefix)

	// The TCP listener is up and authenticating: /v1/health with the token is 200.
	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.TCPBound, "this test is vacuous unless the web listener actually bound")
	tcpBase := "http://" + listeners.TCPBoundAddr
	tokenPath, err := TokenPath()
	require.NoError(t, err)
	token, err := LoadToken(tokenPath)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	client := &http.Client{Timeout: 10 * time.Second}
	status, _ = readBody(t, mustGet(t, client, tcpBase+"/v1/health", token))
	require.Equal(t, http.StatusOK, status,
		"the token must be accepted on the network listener, or the 404 below could be the gate rather than the route table")

	// And the profile is NOT there — authenticated, on loopback, on the listener
	// that shares the daemon's mux.
	status, body = readBody(t, mustGet(t, client, tcpBase+pprofPath, token))
	assert.Equal(t, http.StatusNotFound, status,
		"a pprof profile must be unreachable on the network listener even with the key enabled and a valid token")
	assert.NotContains(t, body, "goroutine profile: total",
		"the network listener must not return profile content under any status")
	assert.Equal(t, unknownRouteMessage(debugPprofPrefix+"goroutine"), envelopeErrorMessage(t, body),
		"the network listener must answer the ordinary unknown-route envelope, the same as any path it does not serve")

	// The same request with NO token is refused by the gate before it reaches any
	// route at all: an unauthenticated peer sees no profile either.
	status, body = readBody(t, mustGet(t, client, tcpBase+pprofPath, ""))
	assert.Equal(t, http.StatusUnauthorized, status, "an unauthenticated peer must never be served a profile")
	assert.NotContains(t, body, "goroutine profile: total")
}

// TestDebugPprof_DefaultOffIsNotFound is invariant 2, stated exactly: with no
// opt-in the route answers a 404 carrying the ordinary unknown-route envelope —
// NOT an empty body, and not a disabled-looking 403. The assertion is
// indistinguishability: the response must equal, byte for byte in its envelope,
// what a path that was never a route returns. Anything weaker advertises a
// surface worth probing on a daemon that opted out.
func TestDebugPprof_DefaultOffIsNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	cfg := config.DefaultConfig()
	require.False(t, cfg.DebugPprof, "the endpoint must be off in the default config")
	cfg.ListenAddr = "" // the unix socket is the whole surface under test here
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	client := unixSocketClient(t)

	// The socket is serving — otherwise every 404 below is a connection artifact.
	status, _ := readBody(t, mustGet(t, client, "http://af/v1/health", ""))
	require.Equal(t, http.StatusOK, status)

	for _, profile := range []string{"heap", "goroutine", "allocs", "profile", "trace", "cmdline"} {
		path := debugPprofPrefix + profile
		status, body := readBody(t, mustGet(t, client, "http://af"+path, ""))
		assert.Equalf(t, http.StatusNotFound, status, "%s must be 404 while the key is off", path)
		assert.NotEmptyf(t, strings.TrimSpace(body), "%s must return the error envelope, never an empty body", path)
		assert.Equalf(t, unknownRouteMessage(path), envelopeErrorMessage(t, body),
			"%s must be indistinguishable from a path that was never a route", path)
	}

	// The index path too — it is the one a prober reaches for first.
	status, body := readBody(t, mustGet(t, client, "http://af"+debugPprofPrefix, ""))
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, unknownRouteMessage(debugPprofPrefix), envelopeErrorMessage(t, body))

	// The oracle: a route that has never existed, answered by the same daemon.
	status, body = readBody(t, mustGet(t, client, "http://af/v1/NeverARoute", ""))
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, unknownRouteMessage("/v1/NeverARoute"), envelopeErrorMessage(t, body),
		"if this changes, the assertions above are comparing against the wrong shape")
}

// TestDebugPprof_EnvOverridesConfig pins the process-level opt-in contract, which
// mirrors the auto-update one (internal/autoupdate): a valid value wins over
// config in BOTH directions, an unset or empty one defers to config, and an
// unparseable one leaves config in force rather than failing open.
func TestDebugPprof_EnvOverridesConfig(t *testing.T) {
	cases := []struct {
		env     string
		set     bool
		cfgOn   bool
		want    bool
		comment string
	}{
		{set: false, cfgOn: false, want: false, comment: "default: off"},
		{set: false, cfgOn: true, want: true, comment: "config alone enables"},
		{env: "1", set: true, cfgOn: false, want: true, comment: "AF_DEBUG_PPROF=1 is the documented spelling"},
		{env: "true", set: true, cfgOn: false, want: true},
		{env: "on", set: true, cfgOn: false, want: true},
		{env: "0", set: true, cfgOn: true, want: false, comment: "the env must be able to turn it OFF for one process"},
		{env: "off", set: true, cfgOn: true, want: false},
		{env: "", set: true, cfgOn: true, want: true, comment: "an empty value defers to config"},
		{env: "  yes  ", set: true, cfgOn: false, want: true, comment: "trimmed and case-folded"},
		{env: "maybe", set: true, cfgOn: true, want: true, comment: "an invalid value must not silently disable"},
		{env: "maybe", set: true, cfgOn: false, want: false, comment: "…nor silently enable"},
	}
	for _, tc := range cases {
		name := "unset"
		if tc.set {
			name = "env=" + tc.env
		}
		t.Run(name+"/cfg", func(t *testing.T) {
			if tc.set {
				t.Setenv(debugPprofEnv, tc.env)
			} else {
				// t.Setenv restores on cleanup; Unsetenv here needs the same, and
				// Setenv-then-Unsetenv is the documented way to get it.
				t.Setenv(debugPprofEnv, "")
				require.NoError(t, os.Unsetenv(debugPprofEnv))
			}
			cfg := config.DefaultConfig()
			cfg.DebugPprof = tc.cfgOn
			assert.Equal(t, tc.want, debugPprofEnabled(cfg), tc.comment)
		})
	}

	// A nil config is the zero-config caller; it must not enable anything.
	assert.False(t, debugPprofEnabled(nil), "a nil config must never enable profiling")
}

// TestWithDebugPprof_DisabledReturnsTheHandlerUnchanged is the structural half of
// invariant 2: off is not "a wrapper that answers 404" but literally the same
// handler, so there is no branch anywhere that could answer differently from a
// build without the feature.
func TestWithDebugPprof_DisabledReturnsTheHandlerUnchanged(t *testing.T) {
	mux := newHTTPMux(&controlServer{})
	// Compared through a checked type assertion rather than require.Same, which
	// reports "Both arguments must be pointers" when the disabled path returns a
	// HandlerFunc — a true failure, described in a way that names nothing.
	disabled := withDebugPprof(mux, false)
	asMux, isMux := disabled.(*http.ServeMux)
	require.Truef(t, isMux && asMux == mux,
		"a disabled mount must return the handler itself, not a wrapper around it; got %T", disabled)

	// Anti-vacuous: the same mux, mounted, does serve the profile — so the
	// assertion above is about the disabled path, not about withDebugPprof
	// being inert.
	rec := httptest.NewRecorder()
	withDebugPprof(mux, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, pprofPath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestWithDebugPprof_ServesNamedProfilesNotTheIndex covers the mount arithmetic in
// isolation. net/http/pprof's Index resolves a profile by cutting its OWN
// "/debug/pprof/" prefix off the request path, so mounting it under /v1 without
// stripping that segment yields a 200 carrying the index page for EVERY profile
// request — a plausible-looking success that contains no profile.
func TestWithDebugPprof_ServesNamedProfilesNotTheIndex(t *testing.T) {
	h := withDebugPprof(newHTTPMux(&controlServer{}), true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, debugPprofPrefix+"goroutine?debug=1", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "goroutine profile: total")
	assert.NotContains(t, rec.Body.String(), "Types of profiles available",
		"the index page must not be served in place of a named profile")

	// The index itself still works at the prefix, so `curl` discovery is intact.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, debugPprofPrefix, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Types of profiles available")

	// An unknown profile name is pprof's own 404, not a panic and not the index.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, debugPprofPrefix+"not-a-profile", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Everything outside the prefix still reaches the wrapped mux untouched.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the wrapper must pass every other path through")
}

// unknownRouteMessage is the mux catch-all's error text for path. Derived from the
// same format the mux uses so this stays a comparison, not a second copy.
func unknownRouteMessage(path string) string {
	return `unknown route "` + path + `"`
}

// envelopeErrorMessage decodes the shared {data,error} envelope and returns its
// error message, failing the test when the body is not one — which is itself the
// assertion for "not an empty body".
func envelopeErrorMessage(t *testing.T, body string) string {
	t.Helper()
	var env apiproto.Envelope
	require.NoErrorf(t, json.Unmarshal([]byte(body), &env), "response body is not the error envelope: %q", body)
	require.NotNilf(t, env.Error, "response envelope carries no error: %q", body)
	return env.Error.Message
}

func mustGet(t *testing.T, client *http.Client, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}
