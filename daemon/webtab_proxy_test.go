package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// newWebTabProxyFixture builds a manager with one started local instance holding a
// web tab pointing at target, and returns the served mux plus the instance's
// stable id (the web client's key) and the web tab index.
func newWebTabProxyFixture(t *testing.T, target string) (mux *http.ServeMux, sessionID string, tabIdx int) {
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
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Kind: "web", URL: target}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	return newHTTPMux(&controlServer{manager: manager}), inst.ID, 1
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

// newStaticFileUpstream starts an upstream that behaves like a static file server
// (python -m http.server): it serves exactly /viewer.html and /assets/x.css and
// 404s every other path — including the "/viewer.html/" that a directory-style
// join of a file target would produce.
func newStaticFileUpstream(t *testing.T, viewerBody, cssBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/viewer.html":
			fmt.Fprint(w, viewerBody)
		case "/assets/x.css":
			fmt.Fprint(w, cssBody)
		default:
			http.Error(w, "404 File not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWebTabProxy_ServesPathBearingTarget is the regression test for the
// user-reported 404: a web tab whose target carries a PATH (http://…/viewer.html)
// must proxy that document at the tab's root, rather than fetching
// "/viewer.html/" (which a static file server 404s), and must resolve a relative
// sub-resource next to the document so its assets load.
func TestWebTabProxy_ServesPathBearingTarget(t *testing.T) {
	const viewerBody = "AF_VIEWER_DOC"
	const cssBody = "AF_VIEWER_CSS"
	upstream := newStaticFileUpstream(t, viewerBody, cssBody)
	mux, id, idx := newWebTabProxyFixture(t, upstream.URL+"/viewer.html")

	cases := []struct {
		name string
		sub  string
		want string
	}{
		// The proxy root must fetch the target path EXACTLY — no trailing slash
		// appended, nothing dropped.
		{name: "proxy root serves the target document", sub: "", want: viewerBody},
		// A relative asset resolves against the target document's directory.
		{name: "relative asset resolves beside the document", sub: "assets/x.css", want: cssBody},
		// A sibling document (what an in-page link produces) resolves too.
		{name: "sibling document resolves", sub: "viewer.html", want: viewerBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%d/%s", id, idx, tc.sub), nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET sub=%q: status = %d, want 200 (body: %s)", tc.sub, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !contains(got, tc.want) {
				t.Fatalf("GET sub=%q: body = %q, want it to contain %q", tc.sub, got, tc.want)
			}
		})
	}
}

// TestResolveUpstreamPath pins the document-reference semantics the proxy resolves
// upstream paths with, including the root-URL target's unchanged behavior.
func TestResolveUpstreamPath(t *testing.T) {
	cases := []struct {
		target string
		rest   string
		want   string
	}{
		// Path-bearing (file) target: root fetches it exactly; relatives resolve
		// against its directory.
		{target: "http://localhost:8899/viewer.html", rest: "", want: "/viewer.html"},
		{target: "http://localhost:8899/viewer.html", rest: "assets/x.css", want: "/assets/x.css"},
		{target: "http://localhost:8899/viewer.html", rest: "style.css", want: "/style.css"},
		{target: "http://localhost:8899/app/viewer.html", rest: "assets/x.css", want: "/app/assets/x.css"},
		// Directory-style target (trailing slash): relatives nest beneath it.
		{target: "http://localhost:8899/app/", rest: "", want: "/app/"},
		{target: "http://localhost:8899/app/", rest: "assets/x.css", want: "/app/assets/x.css"},
		// Root-URL targets keep their existing behavior.
		{target: "http://localhost:8899", rest: "", want: "/"},
		{target: "http://localhost:8899", rest: "assets/x.css", want: "/assets/x.css"},
		{target: "http://localhost:8899/", rest: "", want: "/"},
		{target: "http://localhost:8899/", rest: "assets/x.css", want: "/assets/x.css"},
		// A leading slash on the remainder never escapes to another host: it stays
		// a path on the target.
		{target: "http://localhost:8899/viewer.html", rest: "/assets/x.css", want: "/assets/x.css"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.target)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.target, err)
		}
		got, _ := resolveUpstreamPath(u, tc.rest)
		if got != tc.want {
			t.Errorf("resolveUpstreamPath(%q, %q) = %q, want %q", tc.target, tc.rest, got, tc.want)
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
