package daemon

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/web"
)

// The embedded SPA serving layer for the daemon's HTTP TCP listener (#1592 Phase
// 5 PR2, design §1). Phases 2–4 made the daemon's HTTP mux the whole API; this
// file mounts the browser web client on the catch-all so the SAME listener that
// serves `/v1/...` also serves the app at `/`.
//
// The one load-bearing nuance is the auth split (§1.2): a browser starts with NO
// token, so the static shell (index.html + the JS/CSS bundle) MUST load
// unauthenticated — you cannot paste a token into a page that won't render. But
// every data path stays token-gated exactly as before. webShellHandler encodes
// that split by prefix:
//
//	/v1/...  → the authed API/WS handler (withAuth gate, unchanged)
//	anything else → the embedded static SPA, served WITHOUT a token
//
// This wrapper is applied ONLY to the TCP listener (tcpserver.go). The local unix
// socket keeps its bare mux whose `/` catch-all still 404s, so the web assets are
// never exposed on the socket path — a browser cannot reach a unix socket anyway,
// and keeping them off it means the only new surface is behind the opt-in
// listen_addr token gate.

// webCSP is the Content-Security-Policy served with every static asset. The
// bundle is fully self-contained (esbuild output, no CDN, no external fonts or
// scripts, no off-origin fetch), so `default-src 'self'` is honest and keeps it
// that way: any accidental off-origin dependency introduced later fails loudly in
// the browser instead of silently phoning home.
//
// `style-src 'self' 'unsafe-inline'` is the ONE relaxation (#1592 Phase 5 PR4):
// the attach terminal's xterm.js DOM renderer injects dynamic <style> elements at
// runtime (glyph dimensions + theme colors, computed from the measured font, so
// they cannot be hashed or moved into a static stylesheet). `default-src 'self'`
// alone would block them and break the terminal. The relaxation is scoped to
// STYLES only — every FETCH directive (script-src, connect-src, img-src, font-src)
// still inherits `default-src 'self'`, so the self-contained / no-off-origin
// guarantee the CSP exists to enforce is unchanged; only inline styling is
// permitted, and the app has no untrusted-HTML sink for a style-injection to ride.
// frame-src is deliberately permissive (self for the daemon web-tab proxy, plus
// any http/https origin for an external web tab): a web tab's whole purpose is to
// embed an arbitrary site. This does NOT weaken the self-contained guarantee for
// the SPA itself — script-src/connect-src/etc. still inherit default-src 'self',
// and a framed document has its OWN (separate) CSP; frame-src only governs which
// URLs the shell may embed. Framed sites are sandboxed (no allow-same-origin) so
// they get an opaque origin and cannot reach the shell or its token.
const webCSP = "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self' https: http:"

// webShellHandler wraps the authed API handler so the TCP listener serves the
// embedded SPA on every non-API path while `/v1/...` keeps flowing through the
// token gate untouched. api is the fully-composed authed handler
// (withAuth(mux, gate, cors)); the static branch deliberately sits OUTSIDE it so
// the shell loads with no token.
func webShellHandler(api http.Handler) http.Handler {
	assets := web.Dist()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The API namespace (REST mirror, WS stream, WS events) is entirely under
		// /v1/. Route it to the authed handler so token enforcement, CORS, and the
		// mux's own routing/404 are all preserved verbatim. Everything else is the
		// web app.
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		serveSPA(assets, w, r)
	})
}

// serveSPA serves the embedded web client for a non-API request. It serves the
// concrete asset when the path names a real embedded file, and falls back to
// index.html for any other path so browser-side client routing works (an unknown
// /path deep-link renders the app, not a 404). The static shell is
// unauthenticated by design (§1.2); the CSP header is set on every response so
// the self-contained guarantee is enforced by the browser.
func serveSPA(assets fs.FS, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeHTTPError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("method %s not allowed for %q; use GET", r.Method, r.URL.Path))
		return
	}
	w.Header().Set("Content-Security-Policy", webCSP)
	// nosniff pairs with the CSP: the browser must honor the declared
	// Content-Type (set by serveAsset from the extension) and never sniff a
	// JS/HTML bundle into a different type.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if name == "" || name == "." {
		name = "index.html"
	}
	// path.Clean strips any ".." traversal to a rooted-relative name, but a
	// leading "/" or residual ".." would make fs.FS.Open reject the path; guard
	// explicitly so a hostile path deterministically falls back to the shell
	// rather than erroring.
	if name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		name = "index.html"
	}

	if serveAsset(assets, name, w, r) {
		return
	}
	// SPA fallback: the path names no embedded asset, so serve the app shell for
	// client-side routing.
	serveAsset(assets, "index.html", w, r)
}

// serveAsset writes the embedded file named `name` and returns true, or returns
// false without writing if the name does not resolve to a regular file (missing,
// or a directory) so the caller can fall back to the shell. The asset bytes are
// small (a bundle + a page), so reading them whole and handing http.ServeContent
// a bytes.Reader is simpler than plumbing a Seeker and gives correct
// Content-Type/HEAD/Range handling for free.
func serveAsset(assets fs.FS, name string, w http.ResponseWriter, r *http.Request) bool {
	f, err := assets.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	// Zero modtime → ServeContent emits no Last-Modified and does no caching
	// negotiation; it still infers Content-Type from the extension and handles
	// HEAD. The assets are embedded and versioned with the binary, so per-file
	// modtimes carry no meaning.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}
