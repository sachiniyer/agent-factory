package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWebTabAwareToken_CookieScopedToWebtabPath locks the security scope of the
// af_webtab_token cookie: it is honored ONLY for requests under /v1/webtab/, never
// for the state-changing RPC surface, so it can't become an ambient CSRF credential
// on the general API. The Authorization header / ?access_token query still work
// everywhere.
func TestWebTabAwareToken_CookieScopedToWebtabPath(t *testing.T) {
	cookie := &http.Cookie{Name: webtabTokenCookie, Value: "cookie-tok"}

	// Cookie-only request under the webtab prefix: honored. (The path is
	// /v1/webtab/<sessionId>/<tabId>/… — the tab segment is a stable id since
	// #1810; the cookie's scope is a plain prefix match either way.)
	webReq := httptest.NewRequest(http.MethodGet, "/v1/webtab/sess/a1b2c3d4e5f60718/assets/app.js", nil)
	webReq.AddCookie(cookie)
	if got := webTabAwareToken(webReq); got != "cookie-tok" {
		t.Fatalf("webtab path: token = %q, want cookie-tok", got)
	}

	// The SAME cookie on a non-webtab route: ignored (returns "").
	rpcReq := httptest.NewRequest(http.MethodPost, "/v1/KillSession", nil)
	rpcReq.AddCookie(cookie)
	if got := webTabAwareToken(rpcReq); got != "" {
		t.Fatalf("non-webtab path: token = %q, want empty (cookie must not be honored)", got)
	}

	// Header token wins and works on any route.
	hdrReq := httptest.NewRequest(http.MethodPost, "/v1/KillSession", nil)
	hdrReq.Header.Set("Authorization", "Bearer header-tok")
	if got := webTabAwareToken(hdrReq); got != "header-tok" {
		t.Fatalf("header token: got %q, want header-tok", got)
	}
}

// TestWebTabAwareToken_UsesPrivateQueryParam is the P1 token-conflation guard at
// the auth layer: under the webtab path the daemon's credential rides its OWN
// query param, and a proxied app's ?access_token= must NOT be read as it — that
// misread is exactly what 401'd the iframe before the fix.
func TestWebTabAwareToken_UsesPrivateQueryParam(t *testing.T) {
	// The daemon's private param authorizes.
	r := httptest.NewRequest(http.MethodGet, "/v1/webtab/sess/tab/index.html?af_webtab_token=daemon-tok", nil)
	if got := webTabAwareToken(r); got != "daemon-tok" {
		t.Fatalf("webtab af_webtab_token: got %q, want daemon-tok", got)
	}

	// A proxied app's OWN access_token is invisible to the gate — never mistaken
	// for the daemon credential.
	app := httptest.NewRequest(http.MethodGet, "/v1/webtab/sess/tab/api?access_token=app-value", nil)
	if got := webTabAwareToken(app); got != "" {
		t.Fatalf("webtab app access_token: got %q, want empty (it is the app's, not the daemon's)", got)
	}

	// Both present, app's first: the daemon still reads its own, so auth holds even
	// when the app puts its token in the leading position.
	both := httptest.NewRequest(http.MethodGet, "/v1/webtab/sess/tab/api?access_token=app-value&af_webtab_token=daemon-tok", nil)
	if got := webTabAwareToken(both); got != "daemon-tok" {
		t.Fatalf("webtab both tokens: got %q, want daemon-tok", got)
	}

	// Off the webtab path, ?access_token= is still the general browser/WS fallback.
	gen := httptest.NewRequest(http.MethodGet, "/v1/agent/stream?access_token=general-tok", nil)
	if got := webTabAwareToken(gen); got != "general-tok" {
		t.Fatalf("general access_token: got %q, want general-tok", got)
	}
}
