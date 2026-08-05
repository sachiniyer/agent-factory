package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestPreviewListener_NeverEmitsCORSAllowOrigin is the load-bearing isolation test
// for #1856 step 3b, and it is deliberately the FIRST one: the per-tab origin buys
// cross-tab read isolation only because the browser blocks a cross-origin read, and
// the browser blocks it only while the preview listener emits NO
// Access-Control-Allow-Origin. An operator's cors_allowed_origins is a CONTROL-PLANE
// key — it exists so a separately-hosted UI can call the daemon API — and it must
// not reach the preview origin: one entry there would hand tab A's dev-server JS a
// readable response from tab B's origin, which is the exact escalation the separate
// origin exists to prevent.
//
// It asserts on a request the gate REJECTS, on purpose: applyCORSPolicy runs before
// the gate, so the header is emitted (or not) independently of authorization. A test
// that only checked an authorized response would miss the 401 path, which is the one
// a cross-tab probe actually takes.
func TestPreviewListener_NeverEmitsCORSAllowOrigin(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	// The operator opened CORS for their own separately-hosted control-plane UI.
	cfg.CORSAllowedOrigins = []string{"http://ui.example:3000"}
	m, err := NewManager(cfg)
	require.NoError(t, err)

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, closeHTTP()) }()

	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewBound)
	previewAddr := listeners.PreviewBoundAddr
	controlAddr := listeners.TCPBoundAddr

	client := previewHTTPClient()

	// The control listener honors the allow-list — that is what the key is FOR, and
	// pinning it here is what makes the preview assertion below a real separation
	// rather than a CORS-is-off-everywhere accident.
	ctlReq, err := http.NewRequest(http.MethodGet, "http://"+controlAddr+"/v1/health", nil)
	require.NoError(t, err)
	ctlReq.Header.Set("Origin", "http://ui.example:3000")
	ctlResp, err := client.Do(ctlReq)
	require.NoError(t, err)
	defer func() { _ = ctlResp.Body.Close() }()
	require.Equal(t, "http://ui.example:3000", ctlResp.Header.Get("Access-Control-Allow-Origin"),
		"the control listener must still honor cors_allowed_origins")

	// The preview listener must NOT, for any path or any outcome.
	for _, path := range []string{"/", "/assets/app.js", "/v1/webtab/s/t/"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+previewAddr+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "http://ui.example:3000")
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
			"preview origin %q must never emit Access-Control-Allow-Origin — it is what isolates a cross-tab read", path)
		require.NoError(t, resp.Body.Close())
	}

	// A preflight takes the same path and must be equally silent: a browser that
	// gets an allowed preflight will go on to make the credentialed read.
	preflight, err := http.NewRequest(http.MethodOptions, "http://"+previewAddr+"/", nil)
	require.NoError(t, err)
	preflight.Header.Set("Origin", "http://ui.example:3000")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	pfResp, err := client.Do(preflight)
	require.NoError(t, err)
	defer func() { _ = pfResp.Body.Close() }()
	require.Empty(t, pfResp.Header.Get("Access-Control-Allow-Origin"),
		"a preflight on the preview origin must not be answered with an allow-origin either")
}

// newPreviewOriginFixture brings up a REAL daemon HTTP stack — control listener,
// preview listener, the production gate, shell wrapper and CORS posture — over a
// session holding one web tab per target. It returns the manager, the preview
// listener's bound address, the session id, and each tab's stable id.
//
// The full stack rather than a hand-built handler chain on purpose: every property
// under test here (who may address a tab, what CORS comes back, what the shell
// answers off a preview host) is a property of the WIRING, so a test that rebuilt
// the chain itself would keep passing after the wiring drifted.
func newPreviewOriginFixture(t *testing.T, targets ...string) (m *Manager, previewAddr, sessionID string, tabIDs []string) {
	t.Helper()
	m, sessionID, tabIDs = newPreviewDaemonWithTabs(t, nil, targets...)
	listeners := m.lifecycle.snapshot().listeners
	require.True(t, listeners.PreviewBound, "the preview listener must bind for these tests")
	return m, listeners.PreviewBoundAddr, sessionID, tabIDs
}

// newPreviewDaemonWithTabs is newPreviewOriginFixture's general form: it brings up
// the same real stack over a session holding one web tab per target, with an
// optional hook to tune the config before the manager is built (the strict-auth
// tests need require_token).
//
// Every preview test goes through it because a preview origin is now minted only
// for a LIVE iframe tab — /v1/preview-auth refuses to register an arbitrary
// (session, tab) pair, since the control listener is tokenless by default and a
// cross-origin page could otherwise drive that registry. So a test that wants an
// origin must have a real tab, and getting one has to be one line.
func newPreviewDaemonWithTabs(t *testing.T, tune func(*config.Config), targets ...string) (m *Manager, sessionID string, tabIDs []string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	if tune != nil {
		tune(cfg)
	}
	m, err = NewManager(cfg)
	require.NoError(t, err)

	const title = "previeworigin"
	inst := startedLocalTabInstance(t, m, repo.ID, repoPath, title, "af_"+title+"_agent")
	for i, target := range targets {
		_, err := m.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: target, Name: fmt.Sprintf("web%d", i),
		})
		require.NoError(t, err)
	}
	tabs := inst.GetTabs()
	require.Len(t, tabs, len(targets)+1, "agent tab plus one web tab per target")
	for i := range targets {
		require.NotEmpty(t, tabs[i+1].ID, "the preview origin is derived from the tab's STABLE id")
		tabIDs = append(tabIDs, tabs[i+1].ID)
	}

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeHTTP()) })

	// Deliberately NOT asserting PreviewBound here: the withhold tests tune the
	// config so the listener is disabled or its bind fails, and they need a REAL tab
	// so their assertion is about BOUND-NESS and not about tab existence. Callers
	// that require a bound listener assert it themselves.
	return m, inst.ID, tabIDs
}

// readAllString drains and closes a response body.
func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return string(body)
}

// echoUpstream is a dev server that reports the exact path (and its escaped
// spelling), so a test can prove what the proxy forwarded rather than assume it.
func echoUpstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "server=%s path=%s escaped=%s query=%s cookie=%s host=%s xfh=%s",
			name, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Cookie"), r.Host,
			r.Header.Get("X-Forwarded-Host"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPreviewOrigin_AbsolutePathAssetResolves is #1856's headline: on a per-tab
// preview origin the tab owns the origin ROOT, so an app's ABSOLUTE-path asset
// reaches the dev server's own absolute path.
//
// This is the case the mirrored app-origin route cannot serve and never will: there
// /assets/app.js resolves against the daemon's root, escapes the /v1/webtab/<sid>/<tid>/
// prefix, and 404s (#1811 made that honest rather than silently serving the SPA
// shell). Here the browser's resolution and the dev server's routing happen at the
// same depth by construction, with no Referer heuristic — which is unavailable
// anyway, because the frame's origin makes the browser send none.
func TestPreviewOrigin_AbsolutePathAssetResolves(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	client := previewHTTPClient()

	for _, path := range []string{"/", "/assets/app.js", "/deep/nested/asset.css"} {
		resp := previewHostGet(t, client, previewAddr, host, path)
		require.Equal(t, http.StatusOK, resp.StatusCode, "path %q must reach the dev server", path)
		require.Contains(t, readAllString(t, resp), "server=app path="+path,
			"the tab owns its origin root, so %q must arrive at the dev server VERBATIM", path)
	}
}

// TestPreviewOrigin_TabAOriginNeverServesTabB is the cross-tab isolation core. Each
// tab is a DISTINCT origin, and a tab's origin resolves to exactly one dev server:
// tab A's hostname can never be made to serve tab B's upstream, whatever path is
// requested. Combined with the no-Access-Control-Allow-Origin property pinned above,
// that is what makes a cross-tab READ impossible in a browser — the property the
// single shared origin of steps 2/3a could not provide, because cookies are scoped
// by (host, path) and NOT by port (RFC 6265 §8.5), so a sibling-path fetch on one
// origin still carried the other tab's cookie.
func TestPreviewOrigin_TabAOriginNeverServesTabB(t *testing.T) {
	serverA := echoUpstream(t, "A")
	serverB := echoUpstream(t, "B")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, serverA.URL, serverB.URL)
	client := previewHTTPClient()

	hostA := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	hostB := previewHostOf(t, m, previewAddr, sessionID, tabIDs[1])
	require.NotEqual(t, hostA, hostB, "each tab must get its OWN origin, or there is no isolation to test")

	// Each origin serves its own dev server, and only its own.
	respA := previewHostGet(t, client, previewAddr, hostA, "/x")
	require.Equal(t, http.StatusOK, respA.StatusCode)
	require.Contains(t, readAllString(t, respA), "server=A")

	respB := previewHostGet(t, client, previewAddr, hostB, "/x")
	require.Equal(t, http.StatusOK, respB.StatusCode)
	require.Contains(t, readAllString(t, respB), "server=B")

	// There is no path on A's origin that reaches B — the tab is chosen by HOST, and
	// the path space belongs entirely to A's dev server.
	for _, path := range []string{
		"/v1/webtab/" + sessionID + "/" + tabIDs[1] + "/",
		"/../" + tabIDs[1] + "/",
		"/" + tabIDs[1] + "/",
	} {
		resp := previewHostGet(t, client, previewAddr, hostA, path)
		body := readAllString(t, resp)
		require.NotContains(t, body, "server=B",
			"no path on tab A's origin may reach tab B's dev server (tried %q)", path)
	}
}

// TestPreviewOrigin_ForwardsEscapedPathVerbatim pins the invariant this route keeps
// being re-broken on: the ESCAPED path is forwarded, so a %2F that is DATA inside a
// segment stays a %2F instead of becoming a separator that names a different
// upstream route. It is the same rule the app-origin route holds; sharing one serve
// core (serveWebTabRoute) is what keeps the two from drifting.
func TestPreviewOrigin_ForwardsEscapedPathVerbatim(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])

	resp := previewHostGet(t, previewHTTPClient(), previewAddr, host, "/files/a%2Fb.txt")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readAllString(t, resp)
	require.Contains(t, body, "escaped=/files/a%2Fb.txt",
		"an encoded slash must survive the hop as data inside one segment, not become a separator")
}

// TestPreviewOrigin_RejectsEncodedDotDot pins the other half of the path rule: an
// ENCODED ../ is NOT cleaned by ServeMux (only a literal one is, via a 301), so it
// decodes into the handler intact and must be refused. It matters at least as much
// here as on the mirrored route: the check is what keeps a crafted path from being
// forwarded upstream in a spelling the dev server resolves differently.
func TestPreviewOrigin_RejectsEncodedDotDot(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	client := previewHTTPClient()

	resp := previewHostGet(t, client, previewAddr, host, "/%2E%2E%2Fsecret")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"an encoded .. segment must be refused, not forwarded")
	_ = readAllString(t, resp)

	// A path that merely CONTAINS ".." is legitimate and must still be served (#2104).
	ok := previewHostGet(t, client, previewAddr, host, "/assets/bundle..js")
	require.Equal(t, http.StatusOK, ok.StatusCode)
	require.Contains(t, readAllString(t, ok), "path=/assets/bundle..js")
}

// TestPreviewOrigin_UpstreamSeesNoCredentialAndNoLabel pins what the untrusted dev
// server is allowed to learn. It must never receive an af credential in any
// spelling, and it must never be told the tab's unguessable host label: the proxy
// sets the upstream Host to the TARGET's own, so the capability that addresses this
// preview stays between the browser and the daemon.
func TestPreviewOrigin_UpstreamSeesNoCredentialAndNoLabel(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	label, ok := previewHostLabel(host)
	require.True(t, ok)

	req, err := http.NewRequest(http.MethodGet, "http://"+previewAddr+"/page?doc=1", nil)
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Authorization", "Bearer daemon-bearer-value")
	// The two origins share a cookie jar (cookies are not port-scoped), so a stale af
	// cookie from the app origin can genuinely arrive here. It must not go upstream.
	req.AddCookie(&http.Cookie{Name: webtabTokenCookie, Value: "daemon-cookie"})
	req.AddCookie(&http.Cookie{Name: previewTokenCookie, Value: "retired-preview-cookie"})
	req.AddCookie(&http.Cookie{Name: "app_session", Value: "keep-me"})

	resp, err := previewHTTPClient().Do(req)
	require.NoError(t, err)
	body := readAllString(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Contains(t, body, "app_session=keep-me", "the app's OWN cookies must still reach it")
	require.NotContains(t, body, "daemon-cookie", "the daemon's token cookie must never reach the dev server")
	require.NotContains(t, body, "retired-preview-cookie", "the retired preview cookie must never reach the dev server")
	require.NotContains(t, body, "daemon-bearer-value", "the Authorization header must be dropped upstream")
	require.NotContains(t, body, label, "the dev server must never be told its tab's unguessable host label")
	require.Contains(t, body, "query=doc=1", "the app's own query rides through untouched")

	// X-Forwarded-Host is read out of the echoed body, because the earlier version of
	// this test asserted "the dev server never sees the label" while checking only the
	// Host header — and SetXForwarded was leaking it through this one the whole time.
	fwdHost := ""
	if _, after, found := strings.Cut(body, "xfh="); found {
		fwdHost = after
	}
	require.Equal(t, upstreamHost(t, upstream.URL), fwdHost,
		"X-Forwarded-Host must name the TARGET, not the tab's capability label")
	require.NotContains(t, fwdHost, label,
		"SetXForwarded copies the inbound Host — on this origin that is the credential, "+
			"and a dev server's request log would keep it")
}

// upstreamHost is the host:port of a test upstream's URL.
func upstreamHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

// TestPreviewOrigin_NoHeaderCarriesTheLabelUpstream pins the CLASS, not the one
// header that was found first. The tab's hostname IS its credential, and
// ReverseProxy clones inbound headers — so every header quoting the browser-facing
// origin hands that credential to the untrusted dev server, which writes it straight
// into an ordinary request log (Vite, Django and Rails all log by default).
//
// Host and X-Forwarded-Host were the two already covered. Origin (sent on any
// non-GET fetch the app makes) and Referer (sent on essentially every sub-resource)
// were not, and were reaching the upstream verbatim. This asserts on the WHOLE
// header set rather than naming the ones known today, so a future header that quotes
// the request origin trips it instead of quietly joining the leak.
func TestPreviewOrigin_NoHeaderCarriesTheLabelUpstream(t *testing.T) {
	seen := make(chan http.Header, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		h.Set("Host-Pseudo-Header", r.Host)
		seen <- h
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	label, ok := previewHostLabel(host)
	require.True(t, ok)

	// A browser framing this origin sends both of these on an app's own fetch.
	req, err := http.NewRequest(http.MethodPost, "http://"+previewAddr+"/api/save", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Origin", "http://"+host)
	req.Header.Set("Referer", "http://"+host+"/app/page?doc=1")
	resp, err := previewHTTPClient().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	got := <-seen
	for name, values := range got {
		for _, v := range values {
			require.NotContains(t, v, label,
				"header %s carried the tab's capability label to the dev server", name)
		}
	}

	// Rewritten to the target's own address, not dropped: an Origin-checking CSRF
	// guard must see a value that agrees with its own Host, and Referer's path is
	// ordinary app data worth keeping.
	targetURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	require.Equal(t, "http://"+targetURL.Host, got.Get("Origin"))
	require.Equal(t, "http://"+targetURL.Host+"/app/page?doc=1", got.Get("Referer"),
		"only the ORIGIN part of Referer is secret — its path and query survive")
}

// TestPreviewOrigin_DoesNotLaunderHostileOrigins is the other half of the rewrite,
// and the half that makes it safe rather than merely tidy.
//
// Rewriting EVERY non-empty Origin would convert a hostile one into a trusted one: a
// form POST from another site (or an `Origin: null` from a sandboxed frame) to a
// known — or leaked — preview URL would arrive at the dev server looking same-origin,
// defeating the only signal an upstream CSRF guard has. And since this rewrite exists
// precisely because labels can escape into request logs, "the attacker knows the
// label" is the case it has to survive, not one it may assume away.
//
// A foreign Origin carries no secret of ours — it is the sender's own — so it is
// passed through untouched for the upstream to refuse.
func TestPreviewOrigin_DoesNotLaunderHostileOrigins(t *testing.T) {
	seen := make(chan http.Header, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	targetURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	// A forged X-Forwarded-Proto must not change what counts as "our own origin".
	// If it did, the tab's real http:// Origin would stop matching and be forwarded
	// verbatim — leaking the label this rewrite exists to contain — and the matching
	// https spelling would be laundered instead.
	forged, err := http.NewRequest(http.MethodPost, "http://"+previewAddr+"/api/save", strings.NewReader("{}"))
	require.NoError(t, err)
	forged.Host = host
	forged.Header.Set("X-Forwarded-Proto", "https")
	forged.Header.Set("Origin", "http://"+host)
	fResp, err := previewHTTPClient().Do(forged)
	require.NoError(t, err)
	require.NoError(t, fResp.Body.Close())
	fGot := <-seen
	require.Equal(t, "http://"+targetURL.Host, fGot.Get("Origin"),
		"a forged X-Forwarded-Proto must not make the tab's OWN origin look foreign — that would leak the label")
	label2, ok2 := previewHostLabel(host)
	require.True(t, ok2)
	require.NotContains(t, fGot.Get("Origin"), label2)

	for _, hostile := range []string{"http://evil.example", "null", "https://" + host} {
		req, rerr := http.NewRequest(http.MethodPost, "http://"+previewAddr+"/api/save", strings.NewReader("{}"))
		require.NoError(t, rerr)
		req.Host = host
		req.Header.Set("Origin", hostile)
		req.Header.Set("Referer", hostile+"/attack")
		resp, derr := previewHTTPClient().Do(req)
		require.NoError(t, derr)
		require.NoError(t, resp.Body.Close())

		got := <-seen
		require.Equal(t, hostile, got.Get("Origin"),
			"a foreign Origin must reach the dev server AS IS, so its CSRF guard can refuse it")
		require.NotEqual(t, "http://"+targetURL.Host, got.Get("Origin"),
			"it must never be laundered into one the upstream trusts")
		require.Equal(t, hostile+"/attack", got.Get("Referer"),
			"the same holds for Referer — it is the sender's own, not this tab's secret")
	}
}

// TestPreviewOrigin_RewritesSetCookieToItsOwnHost pins the cookie half of the
// isolation. rewriteSetCookiePaths drops Domain, so a dev server that tries to set
// Domain=localhost — which every per-tab origin would then send, defeating the
// split — is re-scoped to a host-only cookie for its own tab. The Path is left
// alone, because at prefix "" the app already owns the whole path space.
func TestPreviewOrigin_RewritesSetCookieToItsOwnHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "sid=1; Path=/api; Domain=localhost; HttpOnly")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])

	resp := previewHostGet(t, previewHTTPClient(), previewAddr, host, "/api/login")
	defer func() { _ = resp.Body.Close() }()
	require.Len(t, resp.Cookies(), 1)
	c := resp.Cookies()[0]
	require.Equal(t, "sid", c.Name)
	require.Equal(t, "/api", c.Path, "at prefix \"\" the app's own cookie path is already correct")
	require.Empty(t, c.Domain,
		"Domain must be dropped: a Domain=localhost cookie would be sent to EVERY per-tab origin")

	// A preview frame is CROSS-SITE with the SPA, so a Lax or Strict cookie — which is
	// what an app gets by default — is withheld on every request from it, including
	// same-origin ones. Left alone, a cookie-backed app looks permanently logged out
	// the moment preview_listen_addr is enabled, having worked on the mirror.
	require.Equal(t, http.SameSiteNoneMode, c.SameSite,
		"Lax/Strict mean NEVER SENT in a cross-site frame, so the cookie is inert unless rewritten")
	require.Contains(t, resp.Header.Get("Set-Cookie"), "SameSite=None",
		"and it must reach the browser spelled that way, not merely parse as it")
	require.True(t, c.Secure, "SameSite=None requires Secure; *.localhost is trustworthy so the browser takes it")
	require.True(t, c.Partitioned,
		"partitioned keeps this in a jar keyed to the top-level site rather than creating an ambient third-party cookie")
}

// TestMirrorRoute_LeavesAppCookieSemanticsAlone is the other half of the previous
// test, and the reason it exists is that the fix must not reach the route that
// already works. The mirrored preview is same-origin with the SPA, so the app's own
// SameSite choice is honored there and must survive untouched.
func TestMirrorRoute_LeavesAppCookieSemanticsAlone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "sid=1; Path=/api; HttpOnly")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	m, _, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, webtabPathPrefix+sessionID+"/"+tabIDs[0]+"/api/login", nil)
	newHTTPMux(&controlServer{manager: m}).ServeHTTP(rec, req)

	// Asserted on the SERIALIZED header, which is what the browser actually reads.
	// The parsed enum is a trap here: http.SameSiteDefaultMode is 1, while a cookie
	// carrying no SameSite attribute parses to the zero value 0 — so comparing against
	// the named constant tests something other than "the app's attribute is untouched".
	setCookie := rec.Header().Get("Set-Cookie")
	require.NotEmpty(t, setCookie)
	require.NotContains(t, setCookie, "SameSite",
		"the mirror is same-origin with the SPA: the app's cookie semantics are already correct there")
	require.NotContains(t, setCookie, "Partitioned")
	require.NotContains(t, setCookie, "Secure", "and must not be forced Secure on a plain-HTTP mirror")
	require.Contains(t, setCookie, "Path=/v1/webtab/", "it is still re-scoped under the tab's prefix")
}

// TestPreviewOrigin_RootIsTheAppsOwnRoot pins that "/" on a per-tab origin reaches
// the dev server's OWN root, even when the tab targets a subpath.
//
// The mirrored route redirects a bare tab-root hit to the target's path, so a
// prefixed URL starts resolving at the upstream's depth. Carrying that here was
// wrong and this test previously pinned the wrong behavior: with no prefix the path
// already IS the upstream path, so the redirect only meant an app could never
// address its own root — its home link or fetch("/") bounced back to the subpath —
// on an origin it supposedly owns. The client points the initial frame straight at
// the target's path, so nothing needs the redirect here.
func TestPreviewOrigin_RootIsTheAppsOwnRoot(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL+"/app/viewer.html?doc=1")
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])

	resp := previewHostGet(t, noRedirectClient(), previewAddr, host, "/")
	body := readAllString(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"the app owns its origin root — \"/\" must be proxied, not redirected to the target's subpath")
	require.Contains(t, body, "server=app path=/")
	require.Empty(t, resp.Header.Get("Location"))

	// The mirrored route keeps the redirect: that is where a prefix exists to mirror.
	mirrored := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, webtabPathPrefix+sessionID+"/"+tabIDs[0]+"/", nil)
	newHTTPMux(&controlServer{manager: m}).ServeHTTP(mirrored, req)
	require.Equal(t, http.StatusFound, mirrored.Code, "the mirror still redirects — it has a prefix to mirror")
	require.Equal(t, webtabPathPrefix+sessionID+"/"+tabIDs[0]+"/app/viewer.html?doc=1", mirrored.Header().Get("Location"))
}

// TestPreviewOrigin_StripsUpstreamCORSGrants is the other half of cross-tab read
// isolation, and the half that a config-only fix misses. Forcing the DAEMON's
// allow-list empty stops af from granting a read; it does nothing about the dev
// server's own headers, which this proxy relays. A dev server sending
// "Access-Control-Allow-Origin: *" is not exotic — Vite's does it by default — and
// relayed, it lets tab A's JavaScript read tab B's response, which is precisely the
// escalation the per-tab origin exists to prevent.
func TestPreviewOrigin_StripsUpstreamCORSGrants(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "X-Secret")
		w.Header().Set("Timing-Allow-Origin", "*")
		w.Header().Set("X-Secret", "s3cret")
		_, _ = w.Write([]byte("tab B private body"))
	}))
	t.Cleanup(upstream.Close)

	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])

	resp := previewHostGet(t, previewHTTPClient(), previewAddr, host, "/")
	body := readAllString(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "tab B private body", "the app's own content still reaches its own origin")
	for _, h := range corsGrantHeaders {
		require.Empty(t, resp.Header.Get(h),
			"%s from the dev server would let another tab's origin read this response", h)
	}

	// The MIRRORED route is deliberately unchanged: it is same-origin with the SPA
	// and has always relayed these, so this fix does not quietly alter it.
	mirrored := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, webtabPathPrefix+sessionID+"/"+tabIDs[0]+"/", nil)
	newHTTPMux(&controlServer{manager: m}).ServeHTTP(mirrored, req)
	require.Equal(t, "*", mirrored.Header().Get("Access-Control-Allow-Origin"),
		"the app-origin mirror keeps relaying upstream CORS — unchanged by this PR")
}

// TestPreviewOrigin_ClosedTabStopsResolving pins that a preview origin is not a
// permanent grant: the registry maps a label to a tab id, and a closed tab resolves
// to nothing — a clean 404, never a silent bind to whatever took its place (#1810's
// rule, which the id-keyed route already holds and this origin inherits by sharing
// the same target resolution).
func TestPreviewOrigin_ClosedTabStopsResolving(t *testing.T) {
	serverA := echoUpstream(t, "A")
	serverB := echoUpstream(t, "B")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, serverA.URL, serverB.URL)
	client := previewHTTPClient()
	hostA := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])

	require.Equal(t, http.StatusOK, previewHostStatus(t, client, previewAddr, hostA, "/"))

	_, err := m.CloseTab(CloseTabRequest{Title: "previeworigin", RepoID: previewFixtureRepoID(t, m, sessionID), TabName: "web0"})
	require.NoError(t, err)

	resp := previewHostGet(t, client, previewAddr, hostA, "/")
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a closed tab's origin must 404, never resolve to another tab")
	require.NotContains(t, readAllString(t, resp), "server=B")
}

// previewFixtureRepoID reads back the repo id of the fixture's session, so a test
// can drive CloseTab without the fixture having to hand out its whole request shape.
func previewFixtureRepoID(t *testing.T, m *Manager, sessionID string) string {
	t.Helper()
	inst, repoID, _, err := m.resolveStreamSession(sessionID, "")
	require.NoError(t, err)
	require.NotNil(t, inst)
	return repoID
}

// TestPreviewTabHostLabel_Derivation pins the per-tab derivation: deterministic,
// distinct per tab and per secret, DNS-safe, and — critically — the NUL separators
// make the (sid, tid) pair unambiguous, so no two different pairs that concatenate
// to the same string can share an origin. Without them, ("ab","c") and ("a","bc")
// would collide and one tab's origin would address another's dev server.
func TestPreviewTabHostLabel_Derivation(t *testing.T) {
	const secret = "secret-key-material"

	require.Equal(t, previewTabHostLabel(secret, "s1", "t1"), previewTabHostLabel(secret, "s1", "t1"),
		"derivation must be deterministic")
	require.NotEqual(t, previewTabHostLabel(secret, "s1", "t1"), previewTabHostLabel(secret, "s1", "t2"),
		"different tabs in the same session must get different origins")
	require.NotEqual(t, previewTabHostLabel(secret, "s1", "t1"), previewTabHostLabel(secret, "s2", "t1"),
		"the same tab id in different sessions must get different origins")
	require.NotEqual(t, previewTabHostLabel(secret, "ab", "c"), previewTabHostLabel(secret, "a", "bc"),
		"the separators must prevent a concatenation collision between distinct (sid, tid) pairs")
	require.NotEqual(t, previewTabHostLabel(secret, "s1", "t1"), previewTabHostLabel("other-secret", "s1", "t1"),
		"a different secret must derive a different origin (rotation on restart)")

	label := previewTabHostLabel(secret, "s1", "t1")
	require.True(t, isPreviewLabel(label))
	require.LessOrEqual(t, len(label), 63, "a DNS label may not exceed 63 characters")
	require.Regexp(t, `^af[a-z2-7]+$`, label, "the label must be DNS-safe and start with a letter")
}

// TestPreviewHostLabel_Parsing pins the Host parse the gate and the shell wrapper
// both depend on: only the exact shape af mints, directly under .localhost, is a
// per-tab origin. Everything else is a fail-closed miss, which is what makes the
// hostname a credential rather than a routing hint.
func TestPreviewHostLabel_Parsing(t *testing.T) {
	label := previewTabHostLabel("secret", "s1", "t1")

	for _, host := range []string{
		label + ".localhost",
		label + ".localhost:8444",
		strings.ToUpper(label) + ".LOCALHOST:8444", // Host is case-insensitive
		label + ".localhost.:8444",                 // a single trailing root dot
	} {
		got, ok := previewHostLabel(host)
		require.True(t, ok, "%q must parse as a per-tab preview origin", host)
		require.Equal(t, label, got)
	}

	for _, host := range []string{
		"",
		"localhost:8444",
		"127.0.0.1:8444",
		"[::1]:8444",
		label,                        // no suffix at all
		label + ".example.com:8444",  // right label, wrong parent
		label + ".sub.localhost:844", // not directly under the suffix
		"af.localhost:8444",          // af-prefixed but far too short
		label + "x.localhost:8444",   // right prefix, wrong length
		// Right length, but 0/1/8/9 are outside the base32 alphabet, so this can never
		// be a minted label — the alphabet check is what keeps a guess from being
		// mistaken for a shape af produces.
		"af01234567890123456789012345678901.localhost",
	} {
		_, ok := previewHostLabel(host)
		require.False(t, ok, "%q must NOT be read as a per-tab preview origin", host)
	}
}

// TestPreviewOriginRegistry_CapEvictsOldest pins the registry's bound: a caller that
// already holds the daemon bearer cannot grow the map without limit by minting
// origins for ids that name nothing. Eviction is oldest-first and self-heals, since
// the web client re-registers a tab every time it mounts its pane.
func TestPreviewOriginRegistry_CapEvictsOldest(t *testing.T) {
	reg := newPreviewOriginRegistry()
	first := previewTabHostLabel("secret", "s", "t0")
	for i := 0; i <= previewOriginRegistryMax; i++ {
		tid := fmt.Sprintf("t%d", i)
		reg.register(previewTabHostLabel("secret", "s", tid), "s", tid)
	}
	_, ok := reg.lookup(first)
	require.False(t, ok, "the oldest entry must be evicted once the cap is exceeded")

	newest := previewTabHostLabel("secret", "s", fmt.Sprintf("t%d", previewOriginRegistryMax))
	ref, ok := reg.lookup(newest)
	require.True(t, ok, "the newest entry must survive")
	require.Equal(t, previewTabRef{sessionID: "s", tabID: fmt.Sprintf("t%d", previewOriginRegistryMax)}, ref)

	// Re-registering an existing label must not grow the order list (or the cap would
	// evict live entries every time a pane remounted).
	reg2 := newPreviewOriginRegistry()
	for i := 0; i < 5; i++ {
		reg2.register("same-label", "s", "t")
	}
	require.Len(t, reg2.order, 1, "re-registering the same label must not append a duplicate")
}

// TestPreviewOrigin_DoesNotShadowAppPathsOrMethods pins the property the per-tab
// origin exists to give: the app owns the WHOLE path space and every method on it.
//
// The control listener's posture wrapper answers OPTIONS itself (a CORS preflight
// shortcut) and serves /v1/auth-info before the mux. Both are control-plane
// conveniences, and on a preview origin both would silently steal a route from the
// dev server being previewed — an app with its own CORS API or its own /v1 route
// would get af's answer instead of its own, with nothing in the diff to show it.
func TestPreviewOrigin_DoesNotShadowAppPathsOrMethods(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, previewAddr, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)
	host := previewHostOf(t, m, previewAddr, sessionID, tabIDs[0])
	client := previewHTTPClient()

	// A path the control plane reserves is the APP's here.
	resp := previewHostGet(t, client, previewAddr, host, "/v1/auth-info")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readAllString(t, resp)
	require.Contains(t, body, "server=app path=/v1/auth-info",
		"/v1/auth-info belongs to the previewed app on its own origin, not to the daemon")
	require.NotContains(t, body, "auth_required", "the control-plane probe must not answer here")

	// So does OPTIONS, which the CORS preflight shortcut would otherwise swallow.
	req, err := http.NewRequest(http.MethodOptions, "http://"+previewAddr+"/api/thing", nil)
	require.NoError(t, err)
	req.Host = host
	optResp, err := client.Do(req)
	require.NoError(t, err)
	optBody := readAllString(t, optResp)
	require.Equal(t, http.StatusOK, optResp.StatusCode,
		"OPTIONS must reach the dev server, not be answered 204 by the preflight shortcut")
	require.Contains(t, optBody, "server=app path=/api/thing")
}

// TestPreviewOrigin_ExpiredAddressRendersNotice pins what a stale preview origin
// shows. A daemon restart rotates the secret and empties the registry, so every
// iframe left open keeps addressing a host that no longer names a tab — the
// everyday case, and this route is FRAMED, so the reply is rendered AT THE USER.
// A JSON 401 envelope in the pane is exactly what the notice pages exist to avoid.
func TestPreviewOrigin_ExpiredAddressRendersNotice(t *testing.T) {
	upstream := echoUpstream(t, "app")
	_, previewAddr, _, _ := newPreviewOriginFixture(t, upstream.URL)
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)

	// A well-formed label from a PREVIOUS daemon lifetime: right shape, unknown here.
	stale := previewTabHostLabel("a-previous-daemons-secret", "s", "t") + previewHostSuffix + ":" + port
	resp := previewHostGet(t, previewHTTPClient(), previewAddr, stale, "/")
	body := readAllString(t, resp)

	require.Contains(t, body, "Preview address expired",
		"a stale preview origin must render the notice, not a JSON auth envelope")
	require.Contains(t, body, "Reload the af web UI")
	require.NotContains(t, body, `"error"`, "a framed reply must never be the JSON envelope")
	require.NotContains(t, body, "bearer token", "the pane must not talk about a credential the user never sees")
	require.NotContains(t, body, "http-equiv=\"refresh\"",
		"the address can never become valid again — a self-refreshing page would spin forever")
}

// TestPreviewOrigin_ProbeHostAnswersUnauthenticated pins the reachability probe the
// web client needs before it will switch a frame to a per-tab origin.
//
// It answers the one question no server-side signal can: whether THIS BROWSER can
// reach the preview port. Under `ssh -L 8443:127.0.0.1:8443 remote` the page's
// location is loopback while the preview port is unforwarded on the far end, and
// Safari does not resolve *.localhost at all — both indistinguishable from the
// daemon's side. It must be unauthenticated (the client holds no credential for a
// port it has not reached yet) and must disclose nothing beyond its own existence.
func TestPreviewOrigin_ProbeHostAnswersUnauthenticated(t *testing.T) {
	upstream := echoUpstream(t, "app")
	_, previewAddr, _, _ := newPreviewOriginFixture(t, upstream.URL)
	_, port, err := net.SplitHostPort(previewAddr)
	require.NoError(t, err)

	resp := previewHostGet(t, previewHTTPClient(), previewAddr, previewProbeLabel+previewHostSuffix+":"+port, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode, "the probe must answer without any credential")
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"),
		"a cached yes must not outlive a listener that has gone away")
	body := readAllString(t, resp)
	require.Contains(t, body, previewProbeMessage, "the probe page must post the signal the client waits for")
	require.NotContains(t, body, "8443", "it must not advertise the control-plane address")
	require.NotContains(t, body, "server=app", "it must not be proxied to any dev server")

	// The reserved label can never collide with a minted one: it is not a valid tab
	// label, so no tab could ever be addressed at it.
	require.False(t, isPreviewLabel(previewProbeLabel),
		"the probe label must be unable to name a tab")
}

// TestPreviewAuth_DoesNotRefreshOnUnknownSession pins that the mint guard reads only
// the ALREADY-RESTORED map. /v1/preview-auth is a GET, and under the default
// tokenless-loopback posture any cross-origin page can drive it without being able to
// read the reply — so a miss path that runs findSessionByStableID + refreshLocked
// would let that page force repeated disk work with random session ids. The guard
// that stopped the registry being filled must not hand over a cheaper amplifier.
func TestPreviewAuth_DoesNotRefreshOnUnknownSession(t *testing.T) {
	upstream := echoUpstream(t, "app")
	m, _, sessionID, tabIDs := newPreviewOriginFixture(t, upstream.URL)

	// A live tab still validates through the tracked map.
	require.True(t, m.hasIframeTab(sessionID, tabIDs[0]))

	// Unknown ids answer false, and do so from memory. resolveStreamSession is the
	// refreshing path; trackedStreamSession is the one this must use.
	for _, id := range []string{"no-such-session", "", "af-" + sessionID, sessionID + "x"} {
		require.False(t, m.hasIframeTab(id, tabIDs[0]), "unknown session %q must not validate", id)
		require.Nil(t, m.trackedStreamSession(id), "and must not be resolvable from the tracked map")
	}
	// A known session with an unknown tab is equally a miss, with no refresh either.
	require.False(t, m.hasIframeTab(sessionID, "no-such-tab"))
}
