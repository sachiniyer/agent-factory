package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// newWebTabProxyFixture builds a manager with one started local instance holding a
// web tab pointing at target, and returns the served mux plus the instance's
// stable id (the web client's key) and the web tab's STABLE id (#1810 — the proxy
// route's key).
func newWebTabProxyFixture(t *testing.T, target string) (mux *http.ServeMux, sessionID, tabID string) {
	t.Helper()
	m, _, sessionID, ids, _ := newWebTabProxyFixtureN(t, target)
	return m, sessionID, ids[0]
}

// newWebTabProxyFixtureWithInstance is newWebTabProxyFixture plus the tracked
// instance, for tests that must drive its lifecycle state (archived, #1809).
func newWebTabProxyFixtureWithInstance(t *testing.T, target string) (
	mux *http.ServeMux, inst *session.Instance, sessionID, tabID string,
) {
	t.Helper()
	m, inst, sessionID, ids, _ := newWebTabProxyFixtureN(t, target)
	return m, inst, sessionID, ids[0]
}

// newWebTabProxyFixtureN is newWebTabProxyFixture for N web tabs, returning the
// tracked instance plus each tab's stable id in creation order (so tabs sit at
// ordinals 1..N after the agent tab) and a closer that closes the Nth web tab. It
// is what the misroute tests need: they must close a LOWER tab and prove a HIGHER
// one still resolves to its own dev server.
func newWebTabProxyFixtureN(t *testing.T, targets ...string) (
	mux *http.ServeMux, inst *session.Instance, sessionID string, tabIDs []string, closeWebTab func(n int),
) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const title = "webproxy"
	inst = startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	for i, target := range targets {
		if _, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: target, Name: fmt.Sprintf("web%d", i),
		}); err != nil {
			t.Fatalf("CreateTab(web, %q): %v", target, err)
		}
	}
	tabs := inst.GetTabs()
	if len(tabs) != len(targets)+1 {
		t.Fatalf("tabs = %d, want %d (agent + %d web)", len(tabs), len(targets)+1, len(targets))
	}
	for i := range targets {
		id := tabs[i+1].ID
		if id == "" {
			t.Fatalf("web tab %d has no stable id; the proxy route is id-keyed (#1810)", i)
		}
		tabIDs = append(tabIDs, id)
	}
	closeWebTab = func(n int) {
		t.Helper()
		if _, err := manager.CloseTab(CloseTabRequest{
			Title: title, RepoID: repo.ID, TabName: fmt.Sprintf("web%d", n),
		}); err != nil {
			t.Fatalf("CloseTab(web%d): %v", n, err)
		}
	}
	return newHTTPMux(&controlServer{manager: manager}), inst, inst.ID, tabIDs, closeWebTab
}

// proxyGet issues a proxied GET for the remainder `sub` under the tab's prefix.
func proxyGet(t *testing.T, mux *http.ServeMux, sessionID, tabID, sub string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/%s", sessionID, tabID, sub), nil)
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWebTabProxy_RejectsArchivedSession is the #1809 follow-up gate: archive now
// PRESERVES web tabs, which made an archived session the first one whose web tab
// the proxy could resolve. An archived session is inert — its stored target is a
// bare loopback address whose dev server is long gone and whose port may now host
// something else — so the proxy must refuse until a restore. The tab works again
// the moment liveness flips back.
func TestWebTabProxy_RejectsArchivedSession(t *testing.T) {
	const marker = "AF_WEBTAB_ARCHIVED_GATE"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, marker)
	}))
	defer upstream.Close()

	mux, inst, id, tabID := newWebTabProxyFixtureWithInstance(t, upstream.URL)

	// Live: the target proxies through (the control — proving the refusal below is
	// the archived gate and not a broken fixture).
	if rec := proxyGet(t, mux, id, tabID, ""); rec.Code != http.StatusOK {
		t.Fatalf("live web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Archived: refused, and the upstream is never reached.
	inst.SetStatusForTest(session.Archived)
	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("archived web tab: status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "archived") {
		t.Fatalf("archived web tab: body = %q, want an actionable archived message", body)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("archived web tab: the upstream was proxied; an archived session must be inert")
	}

	// Restored: the preserved tab serves again — the gate is state, not a tombstone.
	inst.SetStatusForTest(session.Running)
	rec = proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restored web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("restored web tab: body = %q, want the upstream content", rec.Body.String())
	}
}

// TestWebTabProxy_ServesLoopbackTarget is the headline proxy test: a web tab
// pointing at a loopback HTTP server is reverse-proxied by the daemon, so a
// same-origin GET /v1/webtab/{id}/{tabId}/ returns the server's content.
func TestWebTabProxy_ServesLoopbackTarget(t *testing.T) {
	const marker = "AF_WEBTAB_PROXY_OK"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><body>%s path=%s</body></html>", marker, r.URL.Path)
	}))
	defer upstream.Close()

	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// The trailing-slash root and a sub-path both proxy through. A root-URL target
	// already mirrors itself, so neither redirects.
	for _, sub := range []string{"", "assets/app.js"} {
		rec := proxyGet(t, mux, id, tabID, sub)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", sub, rec.Code, rec.Body.String())
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if got := string(body); !contains(got, marker) {
			t.Fatalf("GET sub=%q: body %q missing marker %q", sub, got, marker)
		}
		// The proxied path is the remainder under the prefix, forwarded verbatim —
		// proof the prefix was stripped and nothing was re-based onto it.
		if got := string(body); !contains(got, "path=/"+sub) {
			t.Fatalf("GET sub=%q: upstream saw wrong path in %q", sub, got)
		}
	}
}

// newStaticFileUpstream starts an upstream that behaves like a static file server
// (python -m http.server) rooted at a subdirectory: it serves exactly
// /app/viewer.html, /app/x.css and /shared.css, and 404s every other path —
// including any path a prefix-mangling proxy would invent.
func newStaticFileUpstream(t *testing.T, viewerBody, cssBody, sharedBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/viewer.html":
			fmt.Fprint(w, viewerBody)
		case "/app/x.css":
			fmt.Fprint(w, cssBody)
		case "/shared.css":
			fmt.Fprint(w, sharedBody)
		default:
			http.Error(w, "404 File not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWebTabProxy_MirrorsSubdirectoryTarget is the load-bearing test for the
// mirror-path URL model, and the one that retires #1806's documented
// subdirectory-target limits.
//
// The browser URL mirrors the upstream path, so the daemon forwards the remainder
// verbatim and every relative URL on the page resolves at the SAME DEPTH the dev
// server serves it at — including a PARENT-relative one, which lands back inside
// the prefix instead of escaping it.
func TestWebTabProxy_MirrorsSubdirectoryTarget(t *testing.T) {
	const viewerBody = "AF_VIEWER_DOC"
	const cssBody = "AF_VIEWER_CSS"
	const sharedBody = "AF_SHARED_CSS"
	upstream := newStaticFileUpstream(t, viewerBody, cssBody, sharedBody)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/viewer.html")

	cases := []struct {
		name string
		sub  string
		want string
	}{
		// The mirrored path fetches the subdirectory document itself — the case
		// #1806 could not serve.
		{name: "subdirectory target document loads", sub: "app/viewer.html", want: viewerBody},
		// A SIBLING asset: the browser resolves "x.css" on .../app/viewer.html to
		// .../app/x.css, which mirrors upstream /app/x.css.
		{name: "sibling asset resolves beside the document", sub: "app/x.css", want: cssBody},
		// A PARENT-relative asset: the browser resolves "../shared.css" to
		// .../<tabId>/shared.css — still inside the prefix (depth is preserved), and
		// upstream /shared.css. This is what a flat prefix could never express.
		{name: "parent-relative asset resolves inside the prefix", sub: "shared.css", want: sharedBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxyGet(t, mux, id, tabID, tc.sub)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", tc.sub, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !contains(got, tc.want) {
				t.Fatalf("GET sub=%q: body = %q, want it to contain %q", tc.sub, got, tc.want)
			}
		})
	}
}

// TestWebTabProxy_RootRedirectsToTargetPath verifies the other half of the mirror
// model: a bare hit on the tab root is redirected to the target's own path, so the
// browser's URL starts mirroring upstream from the first navigation and the
// ?access_token that authorized it survives the hop.
func TestWebTabProxy_RootRedirectsToTargetPath(t *testing.T) {
	upstream := newStaticFileUpstream(t, "doc", "css", "shared")
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/viewer.html")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/?access_token=tok", id, tabID), nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("root: status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	want := fmt.Sprintf("/v1/webtab/%s/%s/app/viewer.html?access_token=tok", id, tabID)
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("root: Location = %q, want %q", got, want)
	}
}

// TestWebTabProxy_RootTargetDoesNotRedirect guards the redirect loop: a root-URL
// target already mirrors itself, so it must be proxied in place rather than
// redirected to its own path forever.
func TestWebTabProxy_RootTargetDoesNotRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "root-ok")
	}))
	defer upstream.Close()

	for _, target := range []string{upstream.URL, upstream.URL + "/"} {
		mux, id, tabID := newWebTabProxyFixture(t, target)
		rec := proxyGet(t, mux, id, tabID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("target %q: status = %d, want 200 (a root target must not redirect)", target, rec.Code)
		}
	}
}

// TestMirrorRootRedirect pins where a bare tab-root request is sent so the browser
// URL mirrors the target, and — critically — which targets must NOT redirect at all
// (a root target redirecting to its own path is an infinite loop).
func TestMirrorRootRedirect(t *testing.T) {
	const prefix = "/v1/webtab/sess/tab-1"
	cases := []struct {
		target     string
		rawQuery   string
		want       string
		wantRedir  bool
		reasonName string
	}{
		// Path-bearing targets mirror their path.
		{target: "http://localhost:8899/viewer.html", want: prefix + "/viewer.html", wantRedir: true},
		{target: "http://localhost:8899/app/viewer.html", want: prefix + "/app/viewer.html", wantRedir: true},
		// A directory-style target keeps its trailing slash, so the app's relative
		// URLs resolve BENEATH it rather than beside it.
		{target: "http://localhost:8899/app/", want: prefix + "/app/", wantRedir: true},
		// Root targets already mirror themselves — no redirect, no loop.
		{target: "http://localhost:8899", wantRedir: false},
		{target: "http://localhost:8899/", wantRedir: false},
		// The token that authorized the top-level navigation rides along.
		{
			target: "http://localhost:8899/app/viewer.html", rawQuery: "access_token=tok",
			want: prefix + "/app/viewer.html?access_token=tok", wantRedir: true,
		},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.target)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.target, err)
		}
		got, ok := mirrorRootRedirect(prefix, u, tc.rawQuery)
		if ok != tc.wantRedir {
			t.Errorf("mirrorRootRedirect(%q): redirect = %v, want %v", tc.target, ok, tc.wantRedir)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("mirrorRootRedirect(%q, %q) = %q, want %q", tc.target, tc.rawQuery, got, tc.want)
		}
	}
}

// TestWebTabProxy_RejectsNonLoopbackTarget verifies the daemon refuses to proxy a
// non-loopback (external) target — no open proxy / SSRF. External URLs are iframed
// directly by the web UI, never routed through the daemon.
func TestWebTabProxy_RejectsNonLoopbackTarget(t *testing.T) {
	mux, id, tabID := newWebTabProxyFixture(t, "https://example.com")

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("external target: status = %d, want 400", rec.Code)
	}
}

// TestWebTabProxy_RejectsNonWebTab verifies proxying a non-web tab (the agent tab)
// is refused, addressed by the agent tab's own stable id.
func TestWebTabProxy_RejectsNonWebTab(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const title = "webproxy"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: upstream.URL}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	mux := newHTTPMux(&controlServer{manager: manager})
	agentTabID := inst.GetTabs()[0].ID

	rec := proxyGet(t, mux, inst.ID, agentTabID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent tab proxy: status = %d, want 404", rec.Code)
	}
}

// TestWebTabProxy_ClosingLowerTabKeepsPreviewOnItsOwnServer is the #1810
// misroute-prevention proof, and the whole reason the route is id-keyed.
//
// With [agent, webA(:A), webB(:B)], an open pane on webB addresses it by id.
// Closing the LOWER tab webA shifts webB from ordinal 2 to 1. An ordinal-keyed
// route would then silently relay a DIFFERENT dev server to a frame that never
// navigated — HTTP 200, no error, wrong app. By id, webB keeps serving webB.
func TestWebTabProxy_ClosingLowerTabKeepsPreviewOnItsOwnServer(t *testing.T) {
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "DEVSERVER_A")
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "DEVSERVER_B")
	}))
	defer serverB.Close()

	mux, _, id, ids, closeWebTab := newWebTabProxyFixtureN(t, serverA.URL, serverB.URL)
	tabA, tabB := ids[0], ids[1]

	// Before: webB's id serves B.
	if rec := proxyGet(t, mux, id, tabB, ""); !contains(rec.Body.String(), "DEVSERVER_B") {
		t.Fatalf("before close: body = %q, want DEVSERVER_B", rec.Body.String())
	}

	// The developer closes an UNRELATED, LOWER tab. Every higher ordinal shifts down.
	closeWebTab(0)

	// After: the SAME url still serves B. This is the assertion an ordinal-keyed
	// route fails — it would return DEVSERVER_A here, with a 200 and no complaint.
	rec := proxyGet(t, mux, id, tabB, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("after close: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "DEVSERVER_B") {
		t.Fatalf("after close: body = %q, want DEVSERVER_B — the pane was repointed at another dev server", got)
	}

	// And the CLOSED tab's id resolves to nothing: a clean 404 naming the id, never
	// a silent bind to whatever now sits at its old ordinal.
	stale := proxyGet(t, mux, id, tabA, "")
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale id: status = %d, want 404 (body: %s)", stale.Code, stale.Body.String())
	}
	if got := stale.Body.String(); !contains(got, tabA) {
		t.Fatalf("stale id: body = %q, want it to name the unknown tab id %q", got, tabA)
	}
}

// TestWebTabProxy_UnknownTabID404s: an id that never existed is refused outright,
// so a stale/garbled address can never resolve to some other tab.
func TestWebTabProxy_UnknownTabID404s(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	mux, id, _ := newWebTabProxyFixture(t, upstream.URL)

	// Note "1" — the old ORDINAL address. There is no ordinal fallback: it is just
	// an unknown id now, and it must 404 rather than resolve to the tab at index 1.
	for _, bogus := range []string{"no-such-id", "1", "0"} {
		rec := proxyGet(t, mux, id, bogus, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("tab id %q: status = %d, want 404", bogus, rec.Code)
		}
	}
}

// TestWebTabProxy_SetsScopedTokenCookie verifies that when a bearer token
// authorizes the request (via ?access_token=), the handler sets the path-scoped
// af_webtab_token cookie so an iframe's sub-resource GETs stay authorized.
func TestWebTabProxy_SetsScopedTokenCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "?access_token=secret-tok")

	found := cookieNamed(rec, webtabTokenCookie)
	if found == nil {
		t.Fatal("expected af_webtab_token cookie to be set for a token-authorized request")
	}
	if found.Value != "secret-tok" || found.Path != webtabPathPrefix || !found.HttpOnly {
		t.Fatalf("cookie = %+v, want value=secret-tok path=%s HttpOnly", found, webtabPathPrefix)
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie SameSite = %v, want Strict", found.SameSite)
	}
}

// TestWebTabProxy_TokenCookieSecureTracksScheme is the #1808 regression test.
//
// The daemon serves PLAIN HTTP, and a browser silently DROPS a Secure cookie
// delivered over http:// to a non-localhost origin — so flagging it
// unconditionally killed the cookie in the one deployment it exists for (a network
// peer with require_token=true), 401ing every iframe sub-resource. Secure must
// therefore track the scheme the request actually arrived over: omitted on the
// daemon's own plain-HTTP listener, set when a front proxy terminated TLS.
func TestWebTabProxy_TokenCookieSecureTracksScheme(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	cases := []struct {
		name       string
		fwdProto   string
		wantSecure bool
	}{
		// The real remote case: plain HTTP straight to the daemon. A Secure cookie
		// here is a dropped cookie.
		{name: "plain HTTP listener omits Secure", fwdProto: "", wantSecure: false},
		// The recommended deployment: TLS terminated at a front proxy, plain HTTP to
		// the daemon. The browser IS on https, so the cookie can and should be Secure.
		{name: "TLS-terminating front proxy sets Secure", fwdProto: "https", wantSecure: true},
		{name: "X-Forwarded-Proto is case-insensitive", fwdProto: "HTTPS", wantSecure: true},
		// A proxy chain appends; the FIRST entry is the original client's scheme.
		{name: "proxy chain uses the first entry", fwdProto: "https, http", wantSecure: true},
		{name: "explicit http omits Secure", fwdProto: "http", wantSecure: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/v1/webtab/%s/%s/?access_token=tok", id, tabID), nil)
			// A NETWORK peer (not loopback) — the only case the cookie exists for.
			req.RemoteAddr = "172.17.0.4:54321"
			if tc.fwdProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			mux.ServeHTTP(rec, req)

			c := cookieNamed(rec, webtabTokenCookie)
			if c == nil {
				t.Fatal("af_webtab_token cookie was not set")
			}
			if c.Secure != tc.wantSecure {
				t.Fatalf("Secure = %v, want %v (X-Forwarded-Proto: %q)", c.Secure, tc.wantSecure, tc.fwdProto)
			}
			// The rest of the cookie's protection is unchanged either way.
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != webtabPathPrefix {
				t.Fatalf("cookie lost its other protections: %+v", c)
			}
		})
	}
}

// TestWebTabProxy_RemotePeerSubResourcesAuthorize is the end-to-end #1808 proof at
// the gate: a NETWORK peer over plain HTTP with require_token=true authorizes the
// top-level navigation with ?access_token, and the cookie it gets back then
// authorizes the iframe's sub-resource GETs — which carry neither header nor query.
// Before the fix the cookie was Secure, the browser dropped it, and every one of
// those 401'd.
func TestWebTabProxy_RemotePeerSubResourcesAuthorize(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "UPSTREAM path=%s", r.URL.Path)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// The daemon's web listener posture: token required, loopback exempt. Our peer
	// is NOT loopback, so it must present the token.
	gate := &authGate{
		expectedToken:  func() (string, error) { return "secret-tok", nil },
		loopbackExempt: true,
	}
	authed := withAuth(mux, gate, nil)

	// 1. The top-level navigation authorizes via ?access_token (an iframe src cannot
	//    set a header) and gets the cookie back.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/?access_token=secret-tok", id, tabID), nil)
	req.RemoteAddr = "172.17.0.4:54321"
	authed.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("top-level nav: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	cookie := cookieNamed(rec, webtabTokenCookie)
	if cookie == nil {
		t.Fatal("no af_webtab_token cookie for the token-authorized navigation")
	}
	if cookie.Secure {
		t.Fatal("cookie is Secure over plain HTTP: a real browser drops it and every sub-resource 401s (#1808)")
	}

	// 2. The framed app's sub-resource GET: no Authorization header, no
	//    ?access_token — only the cookie the browser stored. It must be authorized.
	sub := httptest.NewRecorder()
	subReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/webtab/%s/%s/rel.css", id, tabID), nil)
	subReq.RemoteAddr = "172.17.0.4:54321"
	subReq.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	authed.ServeHTTP(sub, subReq)
	if sub.Code != http.StatusOK {
		t.Fatalf("sub-resource: status = %d, want 200 — the iframe's assets 401 (body: %s)", sub.Code, sub.Body.String())
	}
	if got := sub.Body.String(); !contains(got, "path=/rel.css") {
		t.Fatalf("sub-resource: upstream saw %q, want path=/rel.css", got)
	}

	// 3. The same sub-resource WITHOUT the cookie is still refused — the cookie is a
	//    real credential, not a hole.
	no := httptest.NewRecorder()
	noReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/rel.css", id, tabID), nil)
	noReq.RemoteAddr = "172.17.0.4:54321"
	authed.ServeHTTP(no, noReq)
	if no.Code != http.StatusUnauthorized {
		t.Fatalf("cookieless sub-resource: status = %d, want 401", no.Code)
	}
}

// TestWebTabProxy_ForwardsCookiesBothDirections verifies cookie-backed dev apps
// work in the iframe: the client's app cookies are forwarded upstream (with the
// daemon's own token cookie stripped), and the upstream's Set-Cookie is relayed
// back re-scoped under the tab's proxy path (Domain dropped).
func TestWebTabProxy_ForwardsCookiesBothDirections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the Cookie the upstream received so the test can assert what was forwarded.
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "appsess", Value: "abc", Path: "/", Domain: "localhost"})
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/", id, tabID), nil)
	// The browser sends both the daemon token cookie and the app's own cookie.
	req.Header.Set("Cookie", webtabTokenCookie+"=daemon-tok; appsess=xyz")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Upstream must have seen the app cookie but NOT the daemon token cookie.
	echoed := rec.Header().Get("X-Echo-Cookie")
	if !contains(echoed, "appsess=xyz") {
		t.Fatalf("upstream Cookie %q missing app cookie appsess=xyz", echoed)
	}
	if contains(echoed, webtabTokenCookie) {
		t.Fatalf("upstream Cookie %q must not carry the daemon token cookie", echoed)
	}

	// The upstream Set-Cookie must be relayed back, re-scoped under this tab's proxy
	// path, Domain dropped.
	wantPath := fmt.Sprintf("/v1/webtab/%s/%s/", id, tabID)
	app := cookieNamed(rec, "appsess")
	if app == nil {
		t.Fatal("upstream Set-Cookie (appsess) was not relayed to the client")
	}
	if app.Path != wantPath {
		t.Fatalf("relayed cookie Path = %q, want %q", app.Path, wantPath)
	}
	if app.Domain != "" {
		t.Fatalf("relayed cookie Domain = %q, want empty (defaults to proxy host)", app.Domain)
	}
}

// TestWebTabProxy_SubdirCookieRidesTheMirroredPath is the cookie half of the
// mirror model: a dev app that scopes a cookie to its OWN subdirectory (Path=/app —
// login/session/CSRF cookies routinely do) must have it re-scoped by a pure
// PREFIX-PREPEND, and the result must be a path the browser will actually send the
// cookie back on.
//
// Because the browser path mirrors upstream, Path=/app becomes
// /v1/webtab/<sid>/<tab>/app — exactly the prefix of the requests the app scoped it
// to, so the cookie rides along on /app/* and nothing else. A model that did not
// mirror depth could not express this.
func TestWebTabProxy_SubdirCookieRidesTheMirroredPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/login" {
			http.SetCookie(w, &http.Cookie{Name: "appsess", Value: "abc", Path: "/app"})
		}
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL+"/app/index.html")

	// The app sets a cookie scoped to its own subdirectory.
	rec := proxyGet(t, mux, id, tabID, "app/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", rec.Code)
	}
	c := cookieNamed(rec, "appsess")
	if c == nil {
		t.Fatal("the app's Path=/app cookie was not relayed")
	}
	wantPath := fmt.Sprintf("/v1/webtab/%s/%s/app", id, tabID)
	if c.Path != wantPath {
		t.Fatalf("cookie Path = %q, want %q (a pure prefix-prepend of /app)", c.Path, wantPath)
	}

	// A real browser sends a Path=/v1/webtab/<sid>/<tab>/app cookie on any request
	// under that path — which, mirrored, is exactly the app's own /app/* requests.
	// Prove the round trip reaches upstream.
	follow := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/app/data.json", id, tabID), nil)
	req.AddCookie(&http.Cookie{Name: "appsess", Value: "abc"})
	mux.ServeHTTP(follow, req)
	if follow.Code != http.StatusOK {
		t.Fatalf("follow-up: status = %d, want 200", follow.Code)
	}
	if got := follow.Header().Get("X-Echo-Cookie"); !contains(got, "appsess=abc") {
		t.Fatalf("upstream Cookie = %q, want it to carry appsess=abc on the /app request", got)
	}
	if got := follow.Body.String(); !contains(got, "path=/app/data.json") {
		t.Fatalf("upstream saw %q, want path=/app/data.json", got)
	}
}

// TestWebTabProxy_StripsFramingHeaders verifies the proxy removes a dev server's
// X-Frame-Options and the frame-ancestors CSP directive, so a same-origin preview
// always frames, while leaving other CSP directives intact.
func TestWebTabProxy_StripsFramingHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "" {
		t.Fatalf("X-Frame-Options = %q, want stripped", xfo)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if contains(csp, "frame-ancestors") {
		t.Fatalf("CSP still carries frame-ancestors: %q", csp)
	}
	if !contains(csp, "default-src 'self'") {
		t.Fatalf("CSP lost its other directives: %q", csp)
	}
}

// cookieNamed returns the named cookie from a recorded response, or nil.
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
