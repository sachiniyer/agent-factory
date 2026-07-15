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
