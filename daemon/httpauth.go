package daemon

import (
	"net/http"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The auth + CORS seam for the daemon HTTP/WS surface (#1592 Phase 2 PR5, §4.4).
// It EXISTS now and enforces NOTHING: over the unix socket the peer is trusted
// (filesystem perms are the auth, #1029). Its whole purpose is shape — Phase 3
// adds a TCP+TLS transport with a bearer token, and this is where the
// constant-time token compare and the real CORS policy drop in WITHOUT reshaping
// a single route or handler. Every route (the REST mirror and the new WS routes)
// is served through withAuth, and the WS handshake rides the same path, so the
// token seam already covers both the Authorization header and the browser
// ?access_token= fallback.

// withAuth wraps the daemon mux with the auth + CORS seam. Phase 2: it extracts
// the request's bearer token (header or query fallback) but does not enforce it,
// applies a permissive CORS policy, and answers CORS preflight.
func withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract the token the request presents (Authorization: Bearer … or the
		// ?access_token= WS/browser fallback). Deliberately discarded in Phase 2 —
		// Phase 3 replaces this line with a constant-time compare against the
		// daemon's token and a 401 on mismatch, and nothing else here changes.
		_ = agentproto.TokenFromRequest(r)

		applyCORSPolicy(w, r)
		if r.Method == http.MethodOptions {
			// CORS preflight: answered by the seam before any route runs.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// applyCORSPolicy sets the CORS response headers. Permissive on the unix socket
// now (any origin); Phase 3's policy tightens this for the TCP/TLS transport. It
// is a hook, not a hard-coded rule, precisely so that later change is a policy
// edit here rather than a re-plumb of every route.
func applyCORSPolicy(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
}
