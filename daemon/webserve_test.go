package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// apiSpy stands in for the authed API handler webShellHandler wraps. It records
// that a request reached it and answers 200, so a test can assert which paths are
// routed to the API branch (token-gated) vs the static SPA branch.
type apiSpy struct{ reached bool }

func (s *apiSpy) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.reached = true
	w.Header().Set("X-Api-Reached", "1")
	w.WriteHeader(http.StatusOK)
}

// TestWebShellHandler_RoutesApiVsStatic pins the auth split (#1592 Phase 5 PR2):
// /v1/... flows to the authed API handler untouched, while every other path is
// served the embedded SPA WITHOUT reaching the API — so the static shell needs no
// token but the data plane stays gated.
func TestWebShellHandler_RoutesApiVsStatic(t *testing.T) {
	spy := &apiSpy{}
	srv := httptest.NewServer(webShellHandler(spy))
	defer srv.Close()

	// /v1/ is routed to the API handler.
	spy.reached = false
	resp, err := http.Get(srv.URL + "/v1/Snapshot")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.True(t, spy.reached, "/v1/ path must reach the authed API handler")
	require.Equal(t, "1", resp.Header.Get("X-Api-Reached"))
	// The API branch must NOT carry the static CSP — that is set only on the shell.
	require.Empty(t, resp.Header.Get("Content-Security-Policy"))

	// A non-/v1 path is served the SPA and never touches the API handler.
	spy.reached = false
	resp, err = http.Get(srv.URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.False(t, spy.reached, "static path must not reach the API handler")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `id="app"`, "/ must serve the index.html shell")
}

// TestServeSPA_StaticAssetsAndFallback covers the static branch: the CSP + nosniff
// headers, concrete-asset serving with the right content type, and the SPA
// fallback to index.html for unknown deep-link paths.
func TestServeSPA_StaticAssetsAndFallback(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	// The bundled JS asset: served with the CSP + a JS content type.
	resp, err := http.Get(srv.URL + "/af-web.js")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self' https: http:", resp.Header.Get("Content-Security-Policy"))
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Contains(t, resp.Header.Get("Content-Type"), "javascript")
	require.NotEmpty(t, body)

	// The extracted CSS asset exists and is served with a CSS content type.
	resp, err = http.Get(srv.URL + "/af-web.css")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "css")

	// An unknown deep link falls back to the index.html shell (client-side routing),
	// still with the CSP set.
	resp, err = http.Get(srv.URL + "/sessions/deadbeef")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self' https: http:", resp.Header.Get("Content-Security-Policy"))
	require.Contains(t, string(body), `id="app"`, "unknown path must fall back to index.html")

	// A path traversal attempt is contained: it resolves to the shell, never
	// escapes the embed root.
	resp, err = http.Get(srv.URL + "/../../etc/passwd")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `id="app"`)
}

// TestServeSPA_EscapedAssetIs404NotTheShell is the #1811 regression test.
//
// A previewed dev app's ABSOLUTE-path asset (/assets/app.js — what Vite, CRA,
// Next and any static site emit) resolves against the ORIGIN ROOT, escaping the
// /v1/webtab/ prefix entirely and landing here. The SPA history-fallback used to
// answer it "200 text/html" with af's own shell, so a <script> received an HTML
// document as JavaScript and a stylesheet was aborted on MIME mismatch — the
// preview broke with nothing reporting an error anywhere.
//
// It cannot be RESCUED (the preview iframe's opaque-origin sandbox means the
// browser sends no Referer to attribute it by — see serveSPA), so the requirement
// is that it fails HONESTLY: a 404, never the shell.
func TestServeSPA_EscapedAssetIs404NotTheShell(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	// The exact shapes real dev servers emit.
	for _, p := range []string{
		"/assets/app.js",       // Vite build output
		"/assets/app.css",      // Vite build output
		"/static/js/bundle.js", // CRA/webpack
		"/src/main.tsx",        // Vite dev
		"/favicon.ico",
		"/img/logo.png",
	} {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			require.Equal(t, http.StatusNotFound, resp.StatusCode,
				"%s must 404, not be answered with the SPA shell", p)
			require.NotContains(t, string(body), `id="app"`,
				"%s was answered with the af SPA's own HTML — the #1811 silent failure", p)
			require.NotContains(t, resp.Header.Get("Content-Type"), "text/html",
				"%s must not be answered as HTML", p)
		})
	}
}

// TestServeSPA_DeepLinkStillGetsTheShell guards the other side of the #1811 rule:
// only EXTENSION-BEARING paths are refused. An extension-less path is a real
// client-side route and must still render the app, so the honest-404 change cannot
// break deep linking.
func TestServeSPA_DeepLinkStillGetsTheShell(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	for _, p := range []string{"/", "/sessions/deadbeef", "/projects/my-repo", "/tasks"} {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), `id="app"`, "%s is a client route and must render the shell", p)
		})
	}
}

// TestServeSPA_RejectsNonGet pins that the static branch only answers GET/HEAD; a
// mutating verb on a non-API path is a 405, not a silent index.html.
func TestServeSPA_RejectsNonGet(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/", "text/plain", strings.NewReader("x"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
