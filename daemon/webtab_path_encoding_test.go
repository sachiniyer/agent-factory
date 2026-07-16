package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The proxy's ENCODING contract, and the post-#1858 regressions against it.
//
// The mirror-path model forwards the remainder under the tab prefix "verbatim".
// Verbatim has to mean the BYTES the browser sent, not the string the router
// decoded them into: %2F inside a segment is data, and turning it into a
// separator names a different upstream route. These tests pin that a path
// survives the hop in the browser's own encoding, while the traversal defense
// that reads the DECODED form keeps holding.

// newEchoPathUpstream is an upstream that reports the path and query it was
// actually asked for, in its own escaping — the only way to see what the proxy
// forwarded rather than what it meant to.
func newEchoPathUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "PATH=%s QUERY=%s", r.URL.EscapedPath(), r.URL.RawQuery)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWebTabProxy_PreservesEncodedSlash is the post-#1858 regression test.
//
// ServeMux hands "{rest...}" back percent-DECODED, so /files/a%2Fb arrives as
// "files/a/b" — indistinguishable from a real two-segment path. Rebuilding the
// upstream path from that decoded value asked the dev server for /files/a/b: a
// DIFFERENT route, which lands on another handler or 404s, for any app whose
// identifiers carry an encoded slash as data (an object key, a nested file id, a
// ref like refs%2Fheads%2Fmain).
//
// The daemon's own redirect rewriter (rewriteUpstreamRef) already preserved this
// encoding coming back the other way; this pins the request direction to match.
func TestWebTabProxy_PreservesEncodedSlash(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "files/a%2Fb")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// The %2F must reach the dev server still escaped: decoded, it would name
	// /files/a/b, which is a different resource entirely.
	if want := "PATH=/files/a%2Fb"; !contains(rec.Body.String(), want) {
		t.Fatalf("upstream saw %q, want it to contain %q — the encoded slash was flattened into a separator",
			rec.Body.String(), want)
	}
}

// TestWebTabProxy_PreservesEscapedSpecialChars guards what the decoded-wildcard
// path used to give for free: a literal "?", "#" or "%" in a filename must reach
// upstream still escaped, never reopening as a query/fragment/escape. Rebuilding
// the path from the request's escaped form has to keep that property, not trade
// it for the %2F fix.
func TestWebTabProxy_PreservesEscapedSpecialChars(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	cases := []struct{ name, sub, want string }{
		{name: "literal question mark", sub: "a%3Fb.txt", want: "PATH=/a%3Fb.txt"},
		{name: "literal hash", sub: "a%23b.txt", want: "PATH=/a%23b.txt"},
		{name: "literal percent", sub: "a%25b.txt", want: "PATH=/a%25b.txt"},
		{name: "space", sub: "a%20b.txt", want: "PATH=/a%20b.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxyGet(t, mux, id, tabID, tc.sub)
			if rec.Code != http.StatusOK {
				t.Fatalf("sub=%q: status = %d, want 200 (body: %s)", tc.sub, rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); !contains(got, tc.want) {
				t.Fatalf("sub=%q: upstream saw %q, want it to contain %q", tc.sub, got, tc.want)
			}
		})
	}
}

// TestWebTabProxy_ForwardsQueryButNotTheDaemonToken pins the query half of the
// mirror: the app's own parameters reach the dev server untouched, while the
// daemon's credential — which authorized the iframe's navigation and means nothing
// upstream — is stripped before the hop. The daemon's credential rides its OWN
// param (af_webtab_token), so the app's params are stripped-proof even when one of
// them is literally named access_token (the P1 token-conflation fix).
func TestWebTabProxy_ForwardsQueryButNotTheDaemonToken(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// The target carries its OWN access_token — a real case (a dev app that proxies
	// an authenticated API, or signs its asset URLs) — alongside doc, and the daemon
	// appends its own af_webtab_token last.
	rec := proxyGet(t, mux, id, tabID, "viewer.html?doc=123&access_token=app-value&af_webtab_token=daemon-tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !contains(got, "doc=123") {
		t.Fatalf("upstream saw %q, want the app's own query (doc=123) forwarded", got)
	}
	// The app's OWN access_token must survive — the daemon strips only its private
	// param, never the app's like-named one.
	if !contains(got, "access_token=app-value") {
		t.Fatalf("upstream saw %q, want the app's own access_token=app-value preserved", got)
	}
	// The daemon's credential must never leak upstream.
	if contains(got, "af_webtab_token") || contains(got, "daemon-tok") {
		t.Fatalf("upstream saw %q — the daemon's credential must never leak upstream", got)
	}
}

// TestWebTabProxy_AppAccessTokenAuthorizesOnBothPostures is the P1 auth half of
// the token-conflation fix: a target that carries its OWN ?access_token= must not
// be mistaken for the daemon credential — neither on a network peer (where the
// daemon reads its own token to authorize) nor on a loopback-exempt peer (where no
// daemon token is presented at all).
func TestWebTabProxy_AppAccessTokenAuthorizesOnBothPostures(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	t.Run("network peer: daemon token authorizes, app token rides through", func(t *testing.T) {
		gate := &authGate{
			expectedToken:  func() (string, error) { return "secret-tok", nil },
			loopbackExempt: true,
		}
		authed := withAuth(mux, gate, nil)

		rec := httptest.NewRecorder()
		// The app's access_token comes FIRST — the position that made TokenFromRequest
		// read it as the daemon credential and 401 the iframe before the fix.
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v1/webtab/%s/%s/api?access_token=app-value&af_webtab_token=secret-tok", id, tabID), nil)
		req.RemoteAddr = "172.17.0.4:54321"
		authed.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the app's own access_token was read as the daemon credential (body: %s)",
				rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); !contains(got, "access_token=app-value") {
			t.Fatalf("upstream saw %q, want the app's access_token=app-value forwarded", got)
		}
	})

	t.Run("exempt peer: app token is preserved, not stripped as the daemon's", func(t *testing.T) {
		// A loopback-exempt peer presents no daemon token, so the URL carries only the
		// app's access_token. The strip must leave it alone.
		rec := proxyGet(t, mux, id, tabID, "api?access_token=app-value")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); !contains(got, "access_token=app-value") {
			t.Fatalf("upstream saw %q — an exempt peer's app access_token was stripped as if it were the daemon's", got)
		}
	})
}

// TestWebTabProxy_PreservesSignedQueryByteForByte is the P2 fix: stripping the
// daemon credential must not parse-and-re-encode the rest of the query. A
// signature or an order-sensitive endpoint depends on the exact bytes, and
// url.Values.Encode would sort the keys and turn %20 into + — a different URL
// despite targetQueryOf promising raw preservation.
func TestWebTabProxy_PreservesSignedQueryByteForByte(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	// Keys deliberately NOT in sorted order, a %20 that must not become +, and the
	// daemon token wedged in the MIDDLE to prove the strip is positional-safe.
	rec := proxyGet(t, mux, id, tabID, "sign?z=1&af_webtab_token=tok&a=hello%20world&sig=abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// Byte-for-byte: original order (z,a,sig), %20 intact, daemon token gone.
	if want := "QUERY=z=1&a=hello%20world&sig=abc"; !contains(rec.Body.String(), want) {
		t.Fatalf("upstream saw %q, want it to contain %q — the query was re-encoded, not string-stripped",
			rec.Body.String(), want)
	}
}

// TestWebTabProxy_RejectsEncodedTraversal is the security counterpart of the
// encoding fix, and the reason the traversal check reads the DECODED remainder.
//
// ServeMux cleans a LITERAL "/../" out of a path (it redirects before any handler
// runs), but it does NOT clean an ENCODED one: %2E%2E%2F reaches the handler
// intact. Now that the proxy forwards the request's escaped path rather than a
// re-encoded decoded one, that residue would ride straight through to the dev
// server — so it must be refused here.
func TestWebTabProxy_RejectsEncodedTraversal(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	for _, sub := range []string{
		"%2E%2E%2F%2E%2E%2Fetc/passwd", // fully encoded
		"a/%2E%2E/%2E%2E/etc/passwd",   // encoded dots under a real segment
	} {
		t.Run(sub, func(t *testing.T) {
			rec := proxyGet(t, mux, id, tabID, sub)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("sub=%q: status = %d, want 400 — encoded traversal must not reach upstream (body: %s)",
					sub, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEscapedRestOf pins the remainder-extraction the forwarded path is built
// from, including the case that makes splitting (rather than prefix-trimming) the
// right call: an id carrying an escaped slash must not shift the segment count.
func TestEscapedRestOf(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{name: "plain remainder", path: "/v1/webtab/s/t/app/viewer.html", want: "app/viewer.html", wantOK: true},
		{name: "encoded slash survives", path: "/v1/webtab/s/t/files/a%2Fb", want: "files/a%2Fb", wantOK: true},
		{name: "empty remainder", path: "/v1/webtab/s/t/", want: "", wantOK: true},
		// An id is percent-encoded by the client, so a %2F inside one is NOT a
		// separator in the escaped path and cannot push the remainder off by a
		// segment. This is what a naive strings.Split on the DECODED path would get
		// wrong, and why the prefix is not trimmed textually either.
		{name: "escaped slash inside an id does not shift segments", path: "/v1/webtab/s%2Fx/t%2Fy/app.js", want: "app.js", wantOK: true},
		{name: "deep remainder", path: "/v1/webtab/s/t/a/b/c/d.css", want: "a/b/c/d.css", wantOK: true},
		// Fails closed rather than guessing at a path the route could not produce.
		{name: "too few segments", path: "/v1/webtab/s", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := escapedRestOf(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("escapedRestOf(%q): ok = %v, want %v", tc.path, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("escapedRestOf(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestStripRawQueryParam is the P2 primitive: remove one key from a raw query
// while every surviving segment keeps its exact bytes and position.
func TestStripRawQueryParam(t *testing.T) {
	const key = "af_webtab_token"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty stays empty", raw: "", want: ""},
		{name: "only the daemon token", raw: "af_webtab_token=tok", want: ""},
		{name: "daemon token is a suffix", raw: "doc=123&af_webtab_token=tok", want: "doc=123"},
		{name: "daemon token in the middle", raw: "a=1&af_webtab_token=tok&b=2", want: "a=1&b=2"},
		{name: "order and escaping preserved verbatim", raw: "z=1&af_webtab_token=t&a=hello%20world", want: "z=1&a=hello%20world"},
		// The app's own like-named param is a DIFFERENT key and must survive.
		{name: "app access_token is not the daemon key", raw: "access_token=app&af_webtab_token=daemon", want: "access_token=app"},
		// Exact key match only — a param that merely contains the name is kept.
		{name: "a superstring key is left alone", raw: "xaf_webtab_token=1", want: "xaf_webtab_token=1"},
		// A bare valueless segment with the key name is still removed.
		{name: "bare valueless key is removed", raw: "af_webtab_token&keep=1", want: "keep=1"},
		{name: "no daemon token present", raw: "a=1&b=2", want: "a=1&b=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripRawQueryParam(tc.raw, key); got != tc.want {
				t.Fatalf("stripRawQueryParam(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestMirrorRootRedirect_CarriesTargetQueryAndEncoding covers the same two
// findings on the OTHER entry point: the redirect that starts the mirror.
//
// A bare hit on the tab root is sent to the target's own path — and must carry the
// target's own QUERY with it, or a tab pointed at ?path=/story/button opens the
// dev server's default view instead of the one the tab names. The path likewise
// keeps its escaping, for the reason TestWebTabProxy_PreservesEncodedSlash pins.
func TestMirrorRootRedirect_CarriesTargetQueryAndEncoding(t *testing.T) {
	const prefix = "/v1/webtab/sess/tab-1"
	cases := []struct {
		name     string
		target   string
		rawQuery string
		want     string
	}{
		{
			name:   "target query rides along",
			target: "http://localhost:8899/viewer.html?doc=123",
			want:   prefix + "/viewer.html?doc=123",
		},
		{
			// Both queries matter and neither may displace the other: the target's
			// names the view, the incoming one carries the daemon token that
			// authorizes the redirected navigation.
			name:     "target query and daemon token both survive",
			target:   "http://localhost:8899/index.html?path=/story/button",
			rawQuery: "af_webtab_token=tok",
			want:     prefix + "/index.html?path=/story/button&af_webtab_token=tok",
		},
		{
			name:   "encoded slash in the target path stays encoded",
			target: "http://localhost:8899/files/a%2Fb",
			want:   prefix + "/files/a%2Fb",
		},
		{
			// The LEADING-position case, which is its own bug: url.Parse gives this
			// target Path "//foo" and EscapedPath "/%2Ffoo", so joining the two
			// independently let TrimLeft eat both decoded slashes but only the one
			// real escaped slash. Path and RawPath then disagreed, net/url dropped
			// the raw form, and the redirect flattened %2Ffoo to foo — the mirror
			// corrupting the very first segment.
			name:   "encoded slash in the FIRST segment stays encoded",
			target: "http://localhost:8899/%2Ffoo",
			want:   prefix + "/%2Ffoo",
		},
		{
			name:   "encoded slash leading a deeper path stays encoded",
			target: "http://localhost:8899/%2Fa/b.css",
			want:   prefix + "/%2Fa/b.css",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.target, err)
			}
			got, ok := mirrorRootRedirect(prefix, u, tc.rawQuery)
			if !ok {
				t.Fatalf("mirrorRootRedirect(%q): ok = false, want a redirect", tc.target)
			}
			if got != tc.want {
				t.Fatalf("mirrorRootRedirect(%q, %q) = %q, want %q", tc.target, tc.rawQuery, got, tc.want)
			}
		})
	}
}
