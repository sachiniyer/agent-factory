package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The cross-site request guard (crosssite.go). The property: a browser request
// from another site never reaches a control-plane route or WebSocket upgrade on a
// TCP listener, in any auth posture, while non-browser clients and the web client's
// own requests are served as before.

const (
	guardCrossSiteSentence = "refused a "
	guardContentSentence   = "Content-Type: application/json"
)

// csDo sends one request to addr with the given headers (Host defaults to addr)
// and returns status, headers and body.
func csDo(t *testing.T, addr, method, path, body string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+loopbackDialAddr(addr)+path, rdr)
	require.NoError(t, err)
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := loopbackHTTPClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(b)
}

// refusedByGuard reports whether a response is the guard's own refusal.
func refusedByGuard(status int, body string) bool {
	return (status == http.StatusForbidden && strings.Contains(body, guardCrossSiteSentence)) ||
		(status == http.StatusUnsupportedMediaType && strings.Contains(body, guardContentSentence))
}

// A cross-site "simple" POST — form-urlencoded, multipart or text/plain, which a
// browser sends from any page with no preflight — is refused on a read route and a
// mutating route, whether or not the browser also sent Origin and fetch metadata.
// The differential: the same route with a JSON body from a non-browser client is
// served.
func TestCrossSite_RefusesSimpleCrossSitePosts(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	evil := map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}
	for _, rt := range []struct{ path, body string }{
		{"/v1/Snapshot", `{}`},
		{"/v1/KillSession", `{"title":"no-such-session"}`},
	} {
		for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain;charset=UTF-8", "multipart/form-data; boundary=x"} {
			hdr := map[string]string{"Content-Type": ct}
			for k, v := range evil {
				hdr[k] = v
			}
			status, _, body := csDo(t, addr, http.MethodPost, rt.path, rt.body, hdr)
			assert.Equal(t, http.StatusForbidden, status, "%s %s from a modern browser: %s", rt.path, ct, body)
			assert.Contains(t, body, "cross-site")

			// A browser with no fetch metadata and no Origin on a form (pre-2019
			// Firefox) still meets the content-type layer.
			status, _, body = csDo(t, addr, http.MethodPost, rt.path, rt.body, map[string]string{"Content-Type": ct})
			assert.Equal(t, http.StatusUnsupportedMediaType, status, "%s %s with no browser headers: %s", rt.path, ct, body)
			assert.Contains(t, body, "with curl, add -H 'Content-Type: application/json'")
		}
		// A body with no Content-Type at all is refused too.
		status, _, body := csDo(t, addr, http.MethodPost, rt.path, rt.body, nil)
		assert.Equal(t, http.StatusUnsupportedMediaType, status, body)

		// Differential: the af CLI's request shape reaches the route.
		status, _, body = csDo(t, addr, http.MethodPost, rt.path, rt.body, map[string]string{"Content-Type": "application/json"})
		assert.False(t, refusedByGuard(status, body), "a JSON POST from a non-browser client must reach %s: %d %s", rt.path, status, body)
	}
}

// Fetch metadata: same-origin and none are admitted; cross-site and — deliberately
// — same-site are refused. Same-site on this machine is another port of the same
// host, where agents run untrusted dev servers.
func TestCrossSite_FetchMetadata(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	for site, want := range map[string]int{
		"cross-site":  http.StatusForbidden,
		"same-site":   http.StatusForbidden,
		"Cross-Site":  http.StatusForbidden,
		"same-origin": http.StatusOK,
		"none":        http.StatusOK,
	} {
		status, _, body := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{}`,
			map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": site})
		assert.Equal(t, want, status, "Sec-Fetch-Site %q: %s", site, body)
	}
}

// Without fetch metadata (an older browser), an Origin must be this daemon's own:
// the Host's name and port, and https when the request is known to be HTTPS.
func TestCrossSite_OriginWithoutFetchMetadata(t *testing.T) {
	m, addr, port := hostGuardListener(t)
	next := *m.Config()
	next.AllowedHosts = []string{"box.tailnet.ts.net"}
	m.storeLivePosture(&next)
	post := func(hdr map[string]string) (int, string) {
		hdr["Content-Type"] = "application/json"
		s, _, b := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{}`, hdr)
		return s, b
	}
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"own origin", map[string]string{"Origin": "http://" + addr}, http.StatusOK},
		{"own origin by name", map[string]string{"Host": "localhost:" + port, "Origin": "http://localhost:" + port}, http.StatusOK},
		{"own origin, case and root dot", map[string]string{"Host": "LOCALHOST.:" + port, "Origin": "http://localhost:" + port}, http.StatusOK},
		{"foreign site", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"opaque origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"another port of this host", map[string]string{"Host": "localhost:" + port, "Origin": "http://localhost:3000"}, http.StatusForbidden},
		{"tailscale serve front door", map[string]string{"Host": "box.tailnet.ts.net", "X-Forwarded-Proto": "https", "Origin": "https://box.tailnet.ts.net"}, http.StatusOK},
		{"plain-http origin on an https request", map[string]string{"Host": "box.tailnet.ts.net", "X-Forwarded-Proto": "https", "Origin": "http://box.tailnet.ts.net"}, http.StatusForbidden},
		{"front door, other port", map[string]string{"Host": "box.tailnet.ts.net", "X-Forwarded-Proto": "https", "Origin": "https://box.tailnet.ts.net:9100"}, http.StatusForbidden},
		{"origin with a path", map[string]string{"Origin": "http://" + addr + "/x"}, http.StatusForbidden},
	} {
		status, body := post(tc.hdr)
		assert.Equal(t, tc.want, status, "%s: %s", tc.name, body)
	}
}

// A browser's own "same-origin" verdict is not second-guessed by comparing Origin
// with Host: a same-host proxy that rewrites Host (nginx's default proxy_pass sends
// its upstream address) would otherwise refuse the operator's own web UI.
func TestCrossSite_SameOriginAttestationSurvivesAHostRewritingProxy(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	status, _, body := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{}`, map[string]string{
		"Content-Type": "application/json", "Origin": "https://af.example.com", "Sec-Fetch-Site": "same-origin",
	})
	assert.Equal(t, http.StatusOK, status, body)
}

// Non-browser clients send no Origin and no fetch metadata: a JSON POST, a POST with
// no body and no Content-Type, and GETs are all served.
func TestCrossSite_NonBrowserClientsAreAdmitted(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	status, _, body := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{}`, map[string]string{"Content-Type": "application/json; charset=utf-8"})
	assert.Equal(t, http.StatusOK, status, body)
	status, _, body = csDo(t, addr, http.MethodPost, "/v1/Snapshot", "", nil)
	assert.Equal(t, http.StatusOK, status, "an empty POST with no Content-Type (curl -X POST): %s", body)
	status, _, body = csDo(t, addr, http.MethodGet, "/v1/health", "", nil)
	assert.Equal(t, http.StatusOK, status, body)
}

// The web client's real request shapes are admitted: af() is a same-origin JSON POST
// carrying X-AF-Client-Version; fetchEditorPortOrigin and the config-assistant reap
// are same-origin body-less POST/DELETE with no Content-Type.
func TestCrossSite_WebClientRequestsAreAdmitted(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	same := func(extra map[string]string) map[string]string {
		h := map[string]string{"Origin": "http://" + addr, "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	status, _, body := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{"repo_id":""}`,
		same(map[string]string{"Content-Type": "application/json", "X-AF-Client-Version": Version()}))
	assert.Equal(t, http.StatusOK, status, body)
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/editor-origin?session=nope"},
		{http.MethodDelete, "/v1/config-assistant"},
	} {
		status, _, body := csDo(t, addr, rt.method, rt.path, "", same(nil))
		assert.False(t, refusedByGuard(status, body), "%s %s: %d %s", rt.method, rt.path, status, body)
	}
}

// An origin the operator listed in cors_allowed_origins keeps its cross-origin
// access — request, preflight and allow-origin header — while any other origin's
// preflight is refused.
func TestCrossSite_CORSConfiguredOriginIsAdmitted(t *testing.T) {
	m, addr, _ := hostGuardListener(t)
	next := *m.Config()
	next.CORSAllowedOrigins = []string{"https://ui.example"}
	m.storeLivePosture(&next)

	hdr := map[string]string{"Content-Type": "application/json", "Origin": "https://ui.example", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors"}
	status, h, body := csDo(t, addr, http.MethodPost, "/v1/Snapshot", `{}`, hdr)
	assert.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, "https://ui.example", h.Get("Access-Control-Allow-Origin"))

	pre := map[string]string{"Origin": "https://ui.example", "Sec-Fetch-Site": "cross-site", "Access-Control-Request-Method": "POST"}
	status, h, _ = csDo(t, addr, http.MethodOptions, "/v1/Snapshot", "", pre)
	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, "https://ui.example", h.Get("Access-Control-Allow-Origin"))

	pre["Origin"] = "https://evil.example"
	status, h, _ = csDo(t, addr, http.MethodOptions, "/v1/Snapshot", "", pre)
	assert.Equal(t, http.StatusForbidden, status, "a foreign preflight is refused outright")
	assert.Empty(t, h.Get("Access-Control-Allow-Origin"))
}

// WebSocket upgrades ride the same guard: a cross-site or foreign-origin handshake
// never upgrades, while the web client's own, a configured CORS origin's and a
// non-browser client's do.
func TestCrossSite_WebSocketUpgrades(t *testing.T) {
	m, addr, _ := hostGuardListener(t)
	next := *m.Config()
	next.CORSAllowedOrigins = []string{"https://ui.example"}
	m.storeLivePosture(&next)
	url := "ws://" + loopbackDialAddr(addr) + "/v1/events"
	dial := func(hdr http.Header) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: loopbackHTTPClient, HTTPHeader: hdr})
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return http.StatusSwitchingProtocols, nil
		}
		if resp != nil {
			return resp.StatusCode, err
		}
		return 0, err
	}
	for _, tc := range []struct {
		name string
		hdr  http.Header
		want int
	}{
		{"cross-site", http.Header{"Origin": {"https://evil.example"}, "Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"websocket"}}, http.StatusForbidden},
		{"same-site", http.Header{"Origin": {"http://localhost:3000"}, "Sec-Fetch-Site": {"same-site"}}, http.StatusForbidden},
		{"foreign origin, no fetch metadata", http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
		{"opaque origin", http.Header{"Origin": {"null"}}, http.StatusForbidden},
		{"web client", http.Header{"Origin": {"http://" + addr}, "Sec-Fetch-Site": {"same-origin"}}, http.StatusSwitchingProtocols},
		{"web client, older browser", http.Header{"Origin": {"http://" + addr}}, http.StatusSwitchingProtocols},
		{"configured CORS origin", http.Header{"Origin": {"https://ui.example"}, "Sec-Fetch-Site": {"cross-site"}}, http.StatusSwitchingProtocols},
		{"non-browser client", nil, http.StatusSwitchingProtocols},
	} {
		got, err := dial(tc.hdr)
		assert.Equal(t, tc.want, got, "%s: %v", tc.name, err)
	}
}

// The web-tab subtree belongs to the framed app: a sandboxed frame's own requests
// are cross-site by definition and go to the app's dev server, so the guard leaves
// them alone. A dot-segment cannot wear that prefix into an RPC route.
func TestCrossSite_WebTabSubtreeIsTheAppsAndDotSegmentsDoNotEscape(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	framed := map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"}
	status, _, body := csDo(t, addr, http.MethodPost, "/v1/webtab/sid/tid/api/save", "x", framed)
	assert.False(t, refusedByGuard(status, body), "a framed app's own request is not the guard's to judge: %d %s", status, body)

	for _, p := range []string{"/v1/Snapshot", "/v1/webtab/../Snapshot", "/v1/webtab/x/../../KillSession", "/v1//KillSession", "/v1/events"} {
		assert.True(t, isControlPlanePath(p), p)
	}
	for _, p := range []string{"/v1/webtab/s/t/", "/v1/webtab/s/t/api/save", "/", "/index.html", "/assets/app.js"} {
		assert.False(t, isControlPlanePath(p), p)
	}
}

// The static shell is a page anyone may link to: a cross-site navigation to it is
// served.
func TestCrossSite_StaticShellIsUntouched(t *testing.T) {
	_, addr, _ := hostGuardListener(t)
	nav := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	status, _, body := csDo(t, addr, http.MethodGet, "/", "", nav)
	assert.Equal(t, http.StatusOK, status, body)
}

// The preview listener's path space is the previewed app's: its sandboxed frame's
// own cross-site, opaque-origin, text/plain POST still reaches the dev server.
func TestCrossSite_PreviewOriginIsUntouched(t *testing.T) {
	m, previewAddr, _, sid, tid := startPreviewDaemon(t)
	minted := previewHostOf(t, m, previewAddr, sid, tid)
	status, _, body := csDo(t, previewAddr, http.MethodPost, "/api/save", "x", map[string]string{
		"Host": minted, "Origin": "null", "Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain",
	})
	assert.False(t, refusedByGuard(status, body), "the preview origin's app owns its requests: %d %s", status, body)
	assert.Contains(t, body, "server=auth", "the request reached the previewed dev server")
}

// The agent-server's static posture carries the guard too, so even a cross-site page
// that somehow held the token could not drive it with a form.
func TestCrossSite_AgentServerListener(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)
	handle, info, err := startTCPListener(newHTTPMux(&controlServer{manager: m}), cfg.ListenAddr, cfg, tokenGatePolicy{}, withoutWebShell, nil, nil)
	require.NoError(t, err)
	defer func() { _ = handle.close() }()

	status, _, body := csDo(t, info.Addr, http.MethodPost, "/v1/Snapshot?access_token="+info.Token, `{}`, map[string]string{
		"Content-Type": "text/plain", "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site",
	})
	assert.Equal(t, http.StatusForbidden, status, body)

	status, _, body = csDo(t, info.Addr, http.MethodPost, "/v1/Snapshot", `{}`, map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer " + info.Token,
	})
	assert.Equal(t, http.StatusOK, status, "the daemon's own agent-server client shape is served: %s", body)
}

// The unix socket is trusted transport no browser can reach: the guard is off there,
// so the documented `curl --unix-socket … -d '{}'` (form-urlencoded) keeps working.
func TestCrossSite_UnixSocketIsUnaffected(t *testing.T) {
	h := withAuth(newHTTPMux(&controlServer{}), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/health", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.False(t, refusedByGuard(rec.Code, rec.Body.String()), "%d %s", rec.Code, rec.Body.String())
}

func TestOriginNamesRequest(t *testing.T) {
	r := func(host, xfp string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/Snapshot", nil)
		req.Host = host
		if xfp != "" {
			req.Header.Set("X-Forwarded-Proto", xfp)
		}
		return req
	}
	yes := []struct{ origin, host, xfp string }{
		{"http://127.0.0.1:8443", "127.0.0.1:8443", ""},
		{"http://[::1]:8443", "[::1]:8443", ""},
		{"https://box.ts.net", "box.ts.net", "https"},
		{"https://box.ts.net", "box.ts.net:443", "https"},
		{"http://box.lan", "box.lan", ""},
		{"https://box.lan", "box.lan", ""}, // a TLS proxy that sends no X-Forwarded-Proto
	}
	for _, tc := range yes {
		assert.True(t, originNamesRequest(tc.origin, r(tc.host, tc.xfp)), "%+v", tc)
	}
	no := []struct{ origin, host, xfp string }{
		{"http://127.0.0.1:3000", "127.0.0.1:8443", ""},
		{"http://localhost:8443", "127.0.0.1:8443", ""},
		{"http://box.ts.net", "box.ts.net", "https"},
		{"http://box.lan:8080", "box.lan", ""},
		{"null", "127.0.0.1:8443", ""},
		{"file://", "127.0.0.1:8443", ""},
		{"http://user@127.0.0.1:8443", "127.0.0.1:8443", ""},
		{"http://127.0.0.1:8443?x", "127.0.0.1:8443", ""},
		{"http://127.0.0.1:8443", "", ""},
	}
	for _, tc := range no {
		assert.False(t, originNamesRequest(tc.origin, r(tc.host, tc.xfp)), "%+v", tc)
	}
}

func TestKeyedLogLimiter(t *testing.T) {
	l := newKeyedLogLimiter(10*time.Minute, 3, 512)
	now := time.Unix(2_000_000, 0)
	l.now = func() time.Time { return now }
	ok, _ := l.allow("a")
	require.True(t, ok)
	ok, _ = l.allow("a")
	require.False(t, ok, "one line per key per interval")
	for _, k := range []string{"b", "c", "d", "e"} {
		l.allow(k)
	}
	now = now.Add(time.Minute)
	ok, suppressed := l.allow("f")
	require.True(t, ok)
	require.Equal(t, 2, suppressed, "the burst capped b..e at two lines; the other two are counted")
}
