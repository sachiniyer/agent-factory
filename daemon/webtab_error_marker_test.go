package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The #1909 gate: an af-GENERATED proxy failure must be distinguishable from an
// upstream-generated one that happens to carry the same status.
//
// The proxy forwards upstream status codes unchanged, so a dev server that answers
// 502 on its own (a framework proxy whose backend is down, a local gateway error
// page) is indistinguishable, by status alone, from the daemon's own
// "upstream-unreachable" 502. The client keyed on the bare status and suppressed
// BOTH — showing af's dead-server fallback in place of a page the app really served.
//
// The marker header is what separates them: the ErrorHandler REPLACES the response
// when the upstream never answered, so the header is present exactly when af
// generated the failure. These tests pin both halves plus the spoof guard.

// TestWebTabProxy_AFGeneratedFailureCarriesMarker is the af-generated half: nothing
// is listening on the target, so the transport fails, the ErrorHandler replaces the
// response, and the marker says so.
func TestWebTabProxy_AFGeneratedFailureCarriesMarker(t *testing.T) {
	// A server that is started and immediately closed leaves a loopback address that
	// is guaranteed free AND guaranteed to refuse — the "dev server isn't up yet"
	// state the fallback exists for.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	mux, sessionID, tabID := newWebTabProxyFixture(t, deadURL)
	rec := proxyGet(t, mux, sessionID, tabID, "")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d for an unreachable upstream", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Header().Get(webtabErrorHeader); got != webtabErrorUpstreamUnreachable {
		t.Errorf("%s = %q, want %q: a client cannot tell af's own 502 from the app's without it (#1909)",
			webtabErrorHeader, got, webtabErrorUpstreamUnreachable)
	}
}

// TestWebTabProxy_UpstreamOwn502ForwardsWithoutMarker is the #1909 bug itself: the
// app ANSWERED — with its own 502 error page — so the frame must render that page.
// The status alone cannot say so; the ABSENCE of the marker is what tells the client
// this 502 is the app's own and not af's.
func TestWebTabProxy_UpstreamOwn502ForwardsWithoutMarker(t *testing.T) {
	const appErrorPage = "<html><body>my framework's own 502 page</body></html>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(appErrorPage))
	}))
	defer upstream.Close()

	mux, sessionID, tabID := newWebTabProxyFixture(t, upstream.URL)
	rec := proxyGet(t, mux, sessionID, tabID, "")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the upstream's own %d forwarded unchanged", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Header().Get(webtabErrorHeader); got != "" {
		t.Errorf("%s = %q on an UPSTREAM-generated 502, want absent: the marker means af generated the failure",
			webtabErrorHeader, got)
	}
	// The app's page is what the frame will render — proof the body was relayed, not
	// replaced by af's envelope.
	if body := rec.Body.String(); body != appErrorPage {
		t.Errorf("body = %q, want the app's own error page %q", body, appErrorPage)
	}
}

// TestWebTabProxy_UpstreamCannotSpoofMarker closes the loop the other two open. The
// marker is a claim the CLIENT trusts ("af generated this"), and everything the
// client trusts the proxy must control: an upstream that sets the header itself
// would otherwise make its OWN answered 502 suppress its page and show af's
// dead-server fallback — #1909 in reverse, and reachable by any dev server that
// happens to echo request headers or picks the same name.
//
// The strip lives in ModifyResponse, which runs on every response the upstream
// actually sent; the ErrorHandler only ever runs when it sent none. The two are
// mutually exclusive, so stripping there cannot erase af's own marker.
func TestWebTabProxy_UpstreamCannotSpoofMarker(t *testing.T) {
	for _, spoof := range []string{
		webtabErrorUpstreamUnreachable,
		"anything-at-all", // the client keys on PRESENCE, so any value is a spoof
	} {
		t.Run(spoof, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(webtabErrorHeader, spoof)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()

			mux, sessionID, tabID := newWebTabProxyFixture(t, upstream.URL)
			rec := proxyGet(t, mux, sessionID, tabID, "")

			if got := rec.Header().Get(webtabErrorHeader); got != "" {
				t.Errorf("%s = %q: an upstream forged af's marker and would fake an af-generated failure",
					webtabErrorHeader, got)
			}
		})
	}
}

// TestWebTabProxy_LowercaseSpoofIsStripped guards the strip against the case the
// header's own name invites. Go canonicalizes header keys on the way in, so a
// lowercase forgery lands under the same canonical key and Del removes it — but the
// browser's Headers.get() is case-insensitive too, so a strip that MISSED the
// lowercase form would leak a spoofable marker to a client that reads it happily.
// Pinned rather than assumed.
func TestWebTabProxy_LowercaseSpoofIsStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Bypass Header.Set's canonicalization to put the raw lowercase form on the
		// wire, exactly as a non-Go dev server would.
		w.Header()["x-af-webtab-error"] = []string{webtabErrorUpstreamUnreachable}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	mux, sessionID, tabID := newWebTabProxyFixture(t, upstream.URL)
	rec := proxyGet(t, mux, sessionID, tabID, "")

	for key, values := range rec.Header() {
		if http.CanonicalHeaderKey(key) == http.CanonicalHeaderKey(webtabErrorHeader) {
			t.Errorf("header %q = %v survived the strip in non-canonical form", key, values)
		}
	}
}
