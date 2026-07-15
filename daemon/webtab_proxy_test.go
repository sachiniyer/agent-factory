package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// newWebTabProxyFixture builds a manager with one started local instance holding a
// web tab pointing at target, and returns the served mux plus the instance's
// stable id (the web client's key) and the web tab index.
func newWebTabProxyFixture(t *testing.T, target string) (mux *http.ServeMux, sessionID string, tabIdx int) {
	t.Helper()
	mux, _, sessionID, tabIdx = newWebTabProxyFixtureWithInstance(t, target)
	return mux, sessionID, tabIdx
}

// newWebTabProxyFixtureWithInstance is newWebTabProxyFixture plus the tracked
// instance, for tests that must drive its lifecycle state (archived, #1809).
func newWebTabProxyFixtureWithInstance(t *testing.T, target string) (mux *http.ServeMux, inst *session.Instance, sessionID string, tabIdx int) {
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
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: target}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	return newHTTPMux(&controlServer{manager: manager}), inst, inst.ID, 1
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

	mux, inst, id, idx := newWebTabProxyFixtureWithInstance(t, upstream.URL)
	path := fmt.Sprintf("/v1/webtab/%s/%d/", id, idx)

	// Live: the target proxies through (the control — proving the refusal below is
	// the archived gate and not a broken fixture).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("live web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Archived: refused, and the upstream is never reached.
	inst.SetStatusForTest(session.Archived)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
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
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restored web tab: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("restored web tab: body = %q, want the upstream content", rec.Body.String())
	}
}

// TestWebTabProxy_ServesLoopbackTarget is the headline proxy test: a web tab
// pointing at a loopback HTTP server is reverse-proxied by the daemon, so a
// same-origin GET /v1/webtab/{id}/{idx}/ returns the server's content.
func TestWebTabProxy_ServesLoopbackTarget(t *testing.T) {
	const marker = "AF_WEBTAB_PROXY_OK"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><body>%s path=%s</body></html>", marker, r.URL.Path)
	}))
	defer upstream.Close()

	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)

	// The trailing-slash root and a sub-path both proxy through.
	for _, sub := range []string{"", "assets/app.js"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/%s", id, idx, sub), nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", sub, rec.Code, rec.Body.String())
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if got := string(body); !contains(got, marker) {
			t.Fatalf("GET sub=%q: body %q missing marker %q", sub, got, marker)
		}
		// The proxied path is the remainder under the prefix, not the /v1/webtab
		// prefix — proof the prefix was stripped before forwarding.
		if got := string(body); !contains(got, "path=/"+sub) {
			t.Fatalf("GET sub=%q: upstream saw wrong path in %q", sub, got)
		}
	}
}

// TestWebTabProxy_RejectsNonLoopbackTarget verifies the daemon refuses to proxy a
// non-loopback (external) target — no open proxy / SSRF. External URLs are iframed
// directly by the web UI, never routed through the daemon.
func TestWebTabProxy_RejectsNonLoopbackTarget(t *testing.T) {
	mux, id, idx := newWebTabProxyFixture(t, "https://example.com")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/", id, idx), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("external target: status = %d, want 400", rec.Code)
	}
}

// TestWebTabProxy_RejectsNonWebTab verifies proxying a non-web tab (the agent tab
// at index 0) is refused.
func TestWebTabProxy_RejectsNonWebTab(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	mux, id, _ := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/0/", id), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent tab proxy: status = %d, want 404", rec.Code)
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
	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/?access_token=secret-tok", id, idx), nil)
	mux.ServeHTTP(rec, req)

	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == webtabTokenCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("expected af_webtab_token cookie to be set for a token-authorized request")
	}
	if found.Value != "secret-tok" || found.Path != webtabPathPrefix || !found.HttpOnly {
		t.Fatalf("cookie = %+v, want value=secret-tok path=%s HttpOnly", found, webtabPathPrefix)
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
	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/", id, idx), nil)
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
	wantPath := fmt.Sprintf("/v1/webtab/%s/%d/", id, idx)
	var app *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "appsess" {
			app = c
		}
	}
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
	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/", id, idx), nil)
	mux.ServeHTTP(rec, req)
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
