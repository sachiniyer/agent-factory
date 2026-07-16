package daemon

import (
	"encoding/json"
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

// TestServeSPA_ManifestContentType pins the one MIME the embedded tree can't get
// right on its own (feat: PWA). Go's built-in table has no .webmanifest, so
// ServeContent would sniff the bytes — and a manifest sniffs as text/plain, because
// it IS just JSON. With nosniff on every asset that mislabelling is authoritative.
// The manifest must arrive as application/manifest+json, parse, and carry the fields
// an install actually depends on.
func TestServeSPA_ManifestContentType(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/manifest.webmanifest")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/manifest+json", resp.Header.Get("Content-Type"))
	// nosniff is what makes the Content-Type above load-bearing rather than a hint.
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	var manifest struct {
		Name       string `json:"name"`
		ShortName  string `json:"short_name"`
		StartURL   string `json:"start_url"`
		Scope      string `json:"scope"`
		Display    string `json:"display"`
		ThemeColor string `json:"theme_color"`
		Icons      []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest), "manifest must be valid JSON")
	require.Equal(t, "Agent Factory", manifest.Name)
	require.Equal(t, "af", manifest.ShortName)
	require.Equal(t, "/", manifest.StartURL)
	require.Equal(t, "/", manifest.Scope)
	require.Equal(t, "standalone", manifest.Display)
	require.NotEmpty(t, manifest.ThemeColor)
	// A maskable icon is the one Chrome/Android cannot substitute for, so pin it
	// here as well as in icons.test.ts — this side proves it survives EMBEDDING,
	// which a Node test reading src/ cannot.
	var maskable bool
	for _, icon := range manifest.Icons {
		if icon.Purpose == "maskable" {
			maskable = true
		}
	}
	require.True(t, maskable, "manifest must declare a maskable icon")
}

// TestServeSPA_PWAAssetsAreEmbedded proves the whole install surface actually made it
// through go:embed and is reachable unauthenticated, which is where it has to be: a
// browser fetches the manifest, the worker, and the icons with no token, before the
// app has ever run. The failure this catches is a build that forgot to copy them into
// dist/ — the Go tests never run `make web-build`, so the embedded tree is whatever
// was committed.
func TestServeSPA_PWAAssetsAreEmbedded(t *testing.T) {
	spy := &apiSpy{}
	srv := httptest.NewServer(webShellHandler(spy))
	defer srv.Close()

	for _, tc := range []struct{ path, wantType string }{
		{"/sw.js", "text/javascript"},
		{"/icons/icon.svg", "image/svg+xml"},
		{"/icons/favicon-16.png", "image/png"},
		{"/icons/favicon-32.png", "image/png"},
		{"/icons/apple-touch-icon-180.png", "image/png"},
		{"/icons/icon-192.png", "image/png"},
		{"/icons/icon-512.png", "image/png"},
		{"/icons/icon-maskable-512.png", "image/png"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			spy.reached = false
			resp, err := http.Get(srv.URL + tc.path)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			_ = resp.Body.Close()

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.False(t, spy.reached, "a static asset must not reach the authed API handler")
			require.NotEmpty(t, body)
			require.Contains(t, resp.Header.Get("Content-Type"), tc.wantType)
		})
	}
}

// TestServeSPA_ServiceWorkerIsStamped guards the cache-busting stamp end to end. A
// worker shipped with its literal __AF_SHELL_VERSION__ placeholder would name one
// permanent cache across every future build, so an auto-updated af could keep serving
// the pre-update shell from it. build.mjs throws if the placeholder goes missing; this
// is the other half — that the committed dist/ was actually built by it.
func TestServeSPA_ServiceWorkerIsStamped(t *testing.T) {
	srv := httptest.NewServer(webShellHandler(&apiSpy{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sw.js")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, string(body), "__AF_SHELL_VERSION__",
		"dist/sw.js still carries the placeholder — run `make web-build`")
	require.Regexp(t, `af-shell-\$\{VERSION\}`, string(body), "sw.js must name a versioned cache")
	require.Regexp(t, `const VERSION = "[0-9a-f]{12}"`, string(body), "sw.js must carry a stamped content hash")
}
