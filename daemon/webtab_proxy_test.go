package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
