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
// daemon's credential — which authorized the iframe's navigation and means
// nothing upstream — is stripped before the hop.
func TestWebTabProxy_ForwardsQueryButNotTheDaemonToken(t *testing.T) {
	upstream := newEchoPathUpstream(t)
	mux, id, tabID := newWebTabProxyFixture(t, upstream.URL)

	rec := proxyGet(t, mux, id, tabID, "viewer.html?doc=123&access_token=tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if !contains(got, "doc=123") {
		t.Fatalf("upstream saw %q, want the app's own query (doc=123) forwarded", got)
	}
	if contains(got, "access_token") {
		t.Fatalf("upstream saw %q — the daemon's credential must never leak upstream", got)
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
			// names the view, the incoming one authorizes the navigation.
			name:     "target query and access token both survive",
			target:   "http://localhost:8899/index.html?path=/story/button",
			rawQuery: "access_token=tok",
			want:     prefix + "/index.html?path=/story/button&access_token=tok",
		},
		{
			name:   "encoded slash in the target path stays encoded",
			target: "http://localhost:8899/files/a%2Fb",
			want:   prefix + "/files/a%2Fb",
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
