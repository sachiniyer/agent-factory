package daemon

import (
	"errors"
	"net/http"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The auth + CORS gate for the daemon HTTP/WS surface (#1592 Phase 2 PR5 seam,
// filled by Phase 3 PR2, §1.4/§1.5). Every route (the REST mirror and the WS
// routes) is served through withAuth, and the WS handshake rides the same path,
// so one gate covers both — the Authorization header and the browser
// ?access_token= fallback — with no route-specific auth logic.
//
// Enforcement is PER-LISTENER. The local unix socket is trusted transport
// (filesystem 0600 perms are the auth, #1029): it passes a nil gate and every
// request is authorized, token ignored. The TCP+TLS listener (PR3) passes a
// real gate and requires a valid bearer token. Because only the unix listener
// exists today, this whole enforcement path is DARK — the gate is always nil at
// runtime and nothing is ever rejected — until PR3 binds TCP with gate != nil.

// errUnauthorized is the failure surfaced (as a 401 envelope) when a gated
// request presents a missing or invalid bearer token. For a REST call it is a
// plain 401; for a WS handshake the 401 pre-empts the upgrade so the client's
// Dial fails — REST and WS auth are the same code path.
var errUnauthorized = errors.New("unauthorized: missing or invalid bearer token")

// authGate decides whether requests on a listener must present the bearer
// token. A nil *authGate means trusted transport (the unix socket): every
// request is authorized and the token is ignored. A non-nil gate enforces the
// token, reading it fresh per auth event so `af token rotate` takes effect for
// new connections without a daemon RPC (§1.3).
type authGate struct {
	// expectedToken returns the daemon's current bearer token. It is called
	// once per auth event (per REST call / per WS handshake, not per byte), so
	// re-reading the small token file keeps rotation live. An error — or an
	// empty token — fails closed: the gate denies (see ConstantTimeEqual).
	expectedToken func() (string, error)
}

// authorize reports whether the request presents the gate's expected bearer
// token. It fails closed on any error resolving the expected token and on an
// empty expected token (a daemon with no token must never accept the empty
// credential), and compares in constant time to close the timing oracle.
func (g *authGate) authorize(r *http.Request) bool {
	want, err := g.expectedToken()
	if err != nil {
		return false
	}
	return ConstantTimeEqual(agentproto.TokenFromRequest(r), want)
}

// withAuth wraps the daemon mux with the auth + CORS gate. gate is nil for the
// trusted unix socket (no token enforcement) and non-nil for the TCP listener.
// corsOrigins is the exact-match browser-origin allow-list (§1.5); empty ⇒ no
// Access-Control-Allow-Origin is emitted.
func withAuth(next http.Handler, gate *authGate, corsOrigins []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		applyCORSPolicy(w, r, corsOrigins)
		if r.Method == http.MethodOptions {
			// CORS preflight carries no credentials — answer it before the gate
			// so cross-origin discovery works for the web client without a token.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if gate != nil && !gate.authorize(r) {
			writeHTTPError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// applyCORSPolicy sets the CORS response headers from the config allow-list
// (§1.5). It echoes the request Origin ONLY when it exactly matches an
// allow-list entry, and then advertises the allowed methods/headers. An empty
// allow-list (the default) emits no Access-Control-Allow-Origin, so no
// cross-origin browser can call the API; same-origin and non-browser clients
// (TUI/CLI, curl) don't do CORS and are unaffected.
func applyCORSPolicy(w http.ResponseWriter, r *http.Request, allowedOrigins []string) {
	origin := r.Header.Get("Origin")
	if origin == "" || !originAllowed(origin, allowedOrigins) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	// The response varies by Origin (we echo it conditionally), so caches must
	// not serve one origin's response to another.
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
}

// originAllowed reports whether origin exactly matches an allow-list entry. No
// wildcards or suffix matching — a browser origin is a full scheme://host[:port]
// and must be listed verbatim.
func originAllowed(origin string, allowedOrigins []string) bool {
	for _, allowed := range allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}
