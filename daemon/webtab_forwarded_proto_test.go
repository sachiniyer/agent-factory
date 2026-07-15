package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The forwarded-scheme contract (#1875): the proxy must tell the upstream what
// scheme the ORIGINAL CLIENT used, not what the daemon-facing hop used.
//
// httputil's SetXForwarded derives X-Forwarded-Proto from the inbound request's
// own TLS state, which OVERWRITES an inbound X-Forwarded-Proto. The daemon's
// listener is plain HTTP by design, so behind a TLS-terminating front proxy — the
// recommended network deployment — every upstream was told an https:// page was
// http://. An app that derives absolute URLs or a WebSocket endpoint from the
// header then emits http://ws:// into an https:// page and the browser blocks it
// as mixed content.

// newForwardedProtoUpstream reports the X-Forwarded-Proto it was handed.
func newForwardedProtoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "xfproto=%s", r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// proxyGetWithHeaders issues a proxied GET carrying extra request headers — the
// front-proxy hop a plain proxyGet cannot express.
func proxyGetWithHeaders(t *testing.T, mux *http.ServeMux, sessionID, tabID, sub string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/webtab/%s/%s/%s", sessionID, tabID, sub), nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWebTabProxy_ForwardsClientSchemeNotTheDaemonHop is the #1875 regression
// test: a TLS-terminating front proxy reports https on a plain-HTTP hop into the
// daemon, and that scheme must survive to the dev server.
func TestWebTabProxy_ForwardsClientSchemeNotTheDaemonHop(t *testing.T) {
	upstream := newForwardedProtoUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGetWithHeaders(t, mux, id, tabID, "", map[string]string{
		"X-Forwarded-Proto": "https",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "xfproto=https") {
		t.Fatalf("upstream saw %q, want xfproto=https — the client's TLS hop was overwritten with the daemon's plain-HTTP one", got)
	}
}

// TestWebTabProxy_ForwardedProtoEdges pins the rest of the rule: the header is
// trusted only to UPGRADE, it reads the FIRST entry of a chain, and a direct
// plain-HTTP client is still reported honestly as http.
func TestWebTabProxy_ForwardedProtoEdges(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			// No front proxy: the daemon-facing hop IS the client's hop.
			name: "a direct plain-HTTP client stays http",
			want: "xfproto=http",
		},
		{
			// A chain appends per hop; the FIRST entry is the scheme the original
			// client actually spoke. Resolved to that single value rather than
			// forwarded verbatim — an upstream that exact-matches "https" would read
			// "https, http" as not-https and the fix would do nothing.
			name:    "a proxy chain reports its first entry",
			headers: map[string]string{"X-Forwarded-Proto": "https, http"},
			want:    "xfproto=https",
		},
		{
			name:    "an explicit http hop stays http",
			headers: map[string]string{"X-Forwarded-Proto": "http"},
			want:    "xfproto=http",
		},
		{
			name:    "the header is matched case-insensitively",
			headers: map[string]string{"X-Forwarded-Proto": "HTTPS"},
			want:    "xfproto=https",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newForwardedProtoUpstream(t)
			mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

			rec := proxyGetWithHeaders(t, mux, id, tabID, "", tc.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !contains(got, tc.want) {
				t.Fatalf("upstream saw %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestVSCodeTab_ForwardsClientScheme is the other half of #1875's scope. ONE
// Rewrite serves both tab kinds and only the X-Forwarded-Prefix lines are gated on
// the kind, so the scheme fix must not be gated either — the reviewer who filed
// this read it as a VS Code-only bug, and the web-tab test above is what proves it
// is not. This is the converse guard: an editor reads the header to build its
// absolute URLs and its WSS endpoint, so it must see https too.
func TestVSCodeTab_ForwardsClientScheme(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	mux := newHTTPMux(&controlServer{manager: manager})

	// The editor spawns on the first request, so reuse the retrying getter to
	// settle it, then re-issue the request that actually carries the front-proxy
	// header.
	_ = getVSCodeProxy(t, mux, id, tabID, "")

	rec := proxyGetWithHeaders(t, mux, id, tabID, "", map[string]string{
		"X-Forwarded-Proto": "https",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !contains(got, "xfproto=https") {
		t.Fatalf("the editor saw %q, want xfproto=https — it would build http:// URLs and a ws:// endpoint into an https:// page", got)
	}
}
