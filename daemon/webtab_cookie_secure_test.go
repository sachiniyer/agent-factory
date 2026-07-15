package daemon

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The web-tab token cookie keeps an iframe's SUB-RESOURCE requests authorized:
// they can carry neither the Authorization header nor the ?access_token query,
// so without the cookie every asset and every WS upgrade under /v1/webtab/ is
// 401'd on a token-gated listener. A VS Code pane is nothing BUT sub-resources
// and a WebSocket, so this is load-bearing for it specifically.

// TestWebTabTokenCookie_NotSecureOverPlainHTTP: af terminates no TLS (#1755), so
// marking the cookie Secure on a plain-HTTP listener made the browser DISCARD it
// — breaking exactly the remote (Tailscale + require_token) case the proxy exists
// to serve, while loopback kept working because browsers trust http://localhost.
func TestWebTabTokenCookie_NotSecureOverPlainHTTP(t *testing.T) {
	c := webTabCookieForRequest(t, func(r *http.Request) {})
	if c == nil {
		t.Fatal("no token cookie was set for a token-bearing request")
	}
	if c.Secure {
		t.Fatal("the token cookie is Secure on a plain-HTTP listener; a browser would refuse to store it, so every iframe sub-resource and WS upgrade would 401")
	}
	if !c.HttpOnly {
		t.Error("the token cookie must stay HttpOnly: script in the framed app must not be able to read the daemon token")
	}
}

// TestWebTabTokenCookie_SecureBehindTLSProxy: when a TLS-terminating proxy fronts
// the daemon, the browser's connection IS encrypted and the cookie must say so.
func TestWebTabTokenCookie_SecureBehindTLSProxy(t *testing.T) {
	c := webTabCookieForRequest(t, func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if c == nil {
		t.Fatal("no token cookie was set")
	}
	if !c.Secure {
		t.Fatal("the token cookie is not Secure behind an HTTPS-terminating proxy; it would be sent in the clear on a downgrade")
	}
	// A proxy chain may append hops; the browser-facing one decides.
	c = webTabCookieForRequest(t, func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https, http")
	})
	if c == nil || !c.Secure {
		t.Fatal("a chained X-Forwarded-Proto must be read from its first (browser-facing) hop")
	}
}

// TestWebTabTokenCookie_BrowserWouldSendItBack is the end-to-end proof, using
// net/http's cookiejar — which implements the SAME Secure rule a browser does —
// so this fails against a Secure cookie over http rather than merely asserting an
// attribute we chose.
func TestWebTabTokenCookie_BrowserWouldSendItBack(t *testing.T) {
	c := webTabCookieForRequest(t, func(r *http.Request) {})
	if c == nil {
		t.Fatal("no token cookie was set")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("http://af.example.test:8443/v1/webtab/sid/1/")
	jar.SetCookies(origin, []*http.Cookie{c})

	got := jar.Cookies(origin)
	if len(got) == 0 {
		t.Fatal("a browser would not send the token cookie back on the next sub-resource request; the framed app would 401")
	}
	if got[0].Name != webtabTokenCookie {
		t.Fatalf("cookie name = %q, want %q", got[0].Name, webtabTokenCookie)
	}
}

// webTabCookieForRequest drives one token-bearing proxy request through the real
// handler and returns the af_webtab_token cookie it set (nil if none). mutate
// adjusts the request before it is served.
func webTabCookieForRequest(t *testing.T, mutate func(*http.Request)) *http.Cookie {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	mux, id, idx := newWebTabProxyFixture(t, upstream.URL)
	rec := httptest.NewRecorder()
	// A token in the query is how an iframe's top-level navigation authorizes:
	// an iframe src can set no header. That request is what mints the cookie.
	req := httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, idx, "")+"?access_token=secret-token", nil)
	req.Host = "af.example.test:8443"
	mutate(req)
	mux.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == webtabTokenCookie {
			return c
		}
	}
	return nil
}
