package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The auth + CORS gate for the daemon HTTP/WS surface (#1592 Phase 2 PR5 seam,
// filled by Phase 3, §1.4/§1.5). Every route (the REST mirror and the WS
// routes) is served through withAuth, and the WS handshake rides the same path,
// so one gate covers both — the Authorization header and the browser
// ?access_token= fallback — with no route-specific auth logic.
//
// Enforcement is PER-LISTENER. The local unix socket is trusted transport
// (filesystem 0600 perms are the auth, #1029): it passes a nil gate and every
// request is authorized, token ignored. The TCP listener passes a real gate
// (startTCPListener) and requires a valid bearer token on every request, so the
// nil-vs-real gate is the whole difference between the two transports.

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
//
// Both bool fields are FAIL-SAFE by their zero value (#1696): the zero authGate
// enforces the token for every peer and grants no exemption. A field only ever
// RELAXES enforcement when explicitly set true, so a hand-built gate (or a
// mis-populated config) can never accidentally weaken auth — it can only be
// weakened on purpose.
type authGate struct {
	// expectedToken returns the daemon's current bearer token. It is called
	// once per auth event (per REST call / per WS handshake, not per byte), so
	// re-reading the small token file keeps rotation live. An error — or an
	// empty token — fails closed: the gate denies (see ConstantTimeEqual).
	expectedToken func() (string, error)

	// expectedTokenForRequest, when non-nil, SUPERSEDES expectedToken: it returns
	// the credential THIS request must present, derived from the request itself. The
	// web-tab PREVIEW listener (#1856) sets it to the per-tab HMAC token keyed on the
	// sid/tid in the path, so the gate authenticates PER TAB — tab A's token is never
	// accepted on tab B's route. Like expectedToken, an error or empty return fails
	// closed. Only the preview listener sets it; every other listener leaves it nil.
	expectedTokenForRequest func(*http.Request) (string, error)

	// tokenDisabled drops token enforcement for ALL peers — the require_token=false
	// posture, which is now the DEFAULT so the daemon-served web UI opens with no
	// login (#1696). Zero value false ⇒ the token is enforced, so this struct's
	// fail-safe posture is unchanged; the relaxation is chosen by the config-derived
	// webListenerPolicy, not inherited. Only the daemon's own listen_addr listener
	// ever sets this; the agent-server never does (its token is mandatory).
	tokenDisabled bool

	// loopbackExempt lets 127.0.0.1/::1 peers skip the token, the same trust the
	// unix socket already grants a same-machine client (#1696). Loopback is judged
	// ONLY from the transport RemoteAddr (isLoopbackRequest), never a header, so it
	// cannot be spoofed by a network peer. Zero value false ⇒ loopback still needs
	// the token: the agent-server sets it false (mandatory token for every peer),
	// the daemon's web listener sets it true.
	loopbackExempt bool

	// presentedToken extracts the credential a request presents. Nil ⇒
	// webTabAwareToken, the daemon/agent-server default (Authorization header, then
	// the ?access_token= / webtab query+cookie fallbacks). The PREVIEW listener
	// (#1856) sets previewPresentedToken instead: it authenticates a SEPARATE
	// ephemeral credential on a distinct query/cookie name, so the daemon bearer and
	// the preview credential can never be read for one another.
	presentedToken func(*http.Request) string
}

// authRequired reports whether THIS request must present a valid bearer token to
// be authorized. It is the single predicate behind BOTH the enforcement decision
// (authorize) and the unauthenticated /v1/auth-info probe, so the token the SPA
// is told to paste and the token the gate actually demands can never disagree.
func (g *authGate) authRequired(r *http.Request) bool {
	if g.tokenDisabled {
		return false
	}
	if g.loopbackExempt && isLoopbackRequest(r) {
		return false
	}
	return true
}

// authorize reports whether the request is allowed through the gate. A request
// that does not require a token (token disabled, or a loopback-exempt peer) is
// authorized unconditionally; otherwise it must present the gate's expected
// bearer token. Token resolution failing, or an empty expected token (a daemon
// with no token must never accept the empty credential), fails closed; the
// compare is constant time to close the timing oracle.
func (g *authGate) authorize(r *http.Request) bool {
	if !g.authRequired(r) {
		return true
	}
	want, err := g.wantToken(r)
	if err != nil {
		return false
	}
	extract := g.presentedToken
	if extract == nil {
		extract = webTabAwareToken
	}
	return ConstantTimeEqual(extract(r), want)
}

// wantToken resolves the credential this request must present. A per-request
// resolver (expectedTokenForRequest — the preview listener's per-tab derivation)
// supersedes the static expectedToken; every other listener uses the static one. An
// empty return from either fails closed at the ConstantTimeEqual in authorize.
func (g *authGate) wantToken(r *http.Request) (string, error) {
	if g.expectedTokenForRequest != nil {
		return g.expectedTokenForRequest(r)
	}
	return g.expectedToken()
}

// webTabAwareToken returns the request's bearer token, resolved differently under
// the web-tab proxy path than on the general API.
//
// The Authorization header wins on every route (a direct API/CLI client). Off the
// webtab path, the ?access_token= query is the browser/WS fallback, as before.
//
// UNDER webtabPathPrefix the daemon's own credential rides a PRIVATE query param
// (webtabTokenQueryParam), never ?access_token= — because the proxy forwards the
// framed app's whole query, and an app that uses its own ?access_token= would
// otherwise be read here as the daemon token and 401'd. A sub-resource GET carries
// neither header nor query, so the scoped af_webtab_token cookie — set by the proxy
// after the top-level navigation authorized — is accepted too. The cookie is NEVER
// honored off the webtab path, so it adds no ambient credential to the
// state-changing API surface (no CSRF vector on the RPC endpoints).
func webTabAwareToken(r *http.Request) string {
	if tok := agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader)); tok != "" {
		return tok
	}
	if r.URL == nil {
		return ""
	}
	if strings.HasPrefix(r.URL.Path, webtabPathPrefix) {
		if tok := r.URL.Query().Get(webtabTokenQueryParam); tok != "" {
			return tok
		}
		if c, err := r.Cookie(webtabTokenCookie); err == nil {
			return c.Value
		}
		return ""
	}
	return agentproto.AccessTokenFromQuery(r.URL.Query())
}

// isLoopbackRequest reports whether the request's peer is a loopback address,
// judged ONLY from the transport RemoteAddr the HTTP server recorded from the
// accepted TCP connection. It NEVER consults a header: X-Forwarded-For,
// X-Real-IP, Forwarded, Host, and Origin are all attacker-controlled, so a
// network peer cannot set one to 127.0.0.1 and skip the token (#1696). The TCP
// source address cannot be forged and still complete the TCP handshake, so this
// is the only spoof-resistant signal.
func isLoopbackRequest(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authInfoPath is the unauthenticated probe the web SPA hits before deciding
// whether to show its paste-token login (#1696). It sits UNDER /v1/ so the TCP
// listener's webShellHandler routes it to this authed handler (where the gate
// lives) rather than to the static shell, but withAuth answers it BEFORE the
// gate so a client with no token can still learn whether it needs one.
const authInfoPath = "/v1/auth-info"

// authInfoResponse is the {auth_required} body of the /v1/auth-info probe. It
// leaks exactly one boolean — whether the requesting peer must present a token —
// and nothing about the token itself.
type authInfoResponse struct {
	AuthRequired bool `json:"auth_required"`
}

// requestPosture is the COMPLETE auth + CORS decision for one request: the gate
// that judges its token and the CORS allow-list that judges its Origin. Bundling
// both into ONE value is the structural guarantee behind #2480 PR2's live-read: a
// listener that reads config per request reads it EXACTLY ONCE and builds one
// requestPosture from that single snapshot (withLivePosture), so a config swap
// landing mid-request can never split one authorization across two config
// generations — require_token from one, require_loopback_token or cors from the
// next. A future term that depends on live config must become a FIELD here; it
// cannot be a second read, because servePosture takes the finished posture and
// reads no config of its own. TestWebListenerReadsConfigExactlyOncePerRequest pins
// that the live path reads the snapshot exactly once.
type requestPosture struct {
	// gate judges the token. Nil ⇒ trusted transport (the unix socket): every
	// request authorized. Non-nil ⇒ the token is enforced per its policy.
	gate *authGate
	// cors is the exact-match browser-origin allow-list; empty ⇒ no
	// Access-Control-Allow-Origin is emitted.
	cors []string
	// previewOrigin marks a listener that carries NO control plane and whose whole
	// path space belongs to the previewed app (#1856 step 3b). It changes three
	// things below, and all three follow from that one fact:
	//
	//   - the OPTIONS short-circuit is withheld. It exists to answer a CORS preflight
	//     before the gate; this listener never emits an allow-origin, so there is no
	//     preflight to succeed — and answering OPTIONS here would steal the method
	//     from a previewed app that handles it (a dev server's own CORS API, WebDAV).
	//   - /v1/auth-info is not answered. It is a control-plane probe about a token
	//     this listener does not use, and the path belongs to the app.
	//   - a denial renders the framed notice rather than the JSON envelope (see
	//     servePosture). This route is shown IN a pane.
	//
	// Zero value false keeps the control-plane behavior, so only the preview
	// listener opts in.
	previewOrigin bool
	// previewWarmingUp reports whether the daemon has not finished restoring. It
	// only ever matters alongside previewOrigin, and it decides WHICH framed page a
	// denial renders — see servePosture. Nil means "not warming up".
	previewWarmingUp func() bool
}

// servePosture runs the auth + CORS flow for one request against a posture the
// caller has ALREADY built from a single config read. It performs no config read
// of its own — that is the whole point: the one read happened once, above.
func servePosture(w http.ResponseWriter, r *http.Request, next http.Handler, p requestPosture) {
	applyCORSPolicy(w, r, p.cors)
	// Both shortcuts below are CONTROL-PLANE conveniences, and both are withheld on a
	// per-tab preview origin, where every path and every method belongs to the
	// previewed app (see requestPosture.previewOrigin). Answering them there would
	// shadow a real route of the dev server being previewed — the exact thing the
	// per-tab origin exists to stop.
	if !p.previewOrigin {
		if r.Method == http.MethodOptions {
			// CORS preflight carries no credentials — answer it before the gate
			// so cross-origin discovery works for the web client without a token.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// The auth-info probe is answered BEFORE the gate: its whole purpose is
		// to let a tokenless client discover whether it needs a token, so gating
		// it would defeat it. It reports the gate's decision FOR THIS PEER, so a
		// loopback client sees auth_required=false while a network client on the
		// same daemon sees true — the same authRequired predicate the gate
		// enforces below, never a different answer.
		if r.URL.Path == authInfoPath {
			writeAuthInfo(w, r, p.gate)
			return
		}
	}
	if p.gate != nil && !p.gate.authorize(r) {
		if p.previewOrigin {
			// A preview origin's denial is RENDERED IN A PANE, so it must not be the
			// JSON envelope. WHICH page depends on whether the address can still become
			// valid, and getting that backwards strands the frame either way.
			//
			// WARMING UP: the listener binds long before RestoreInstances (#829), so an
			// editor iframe can navigate while the daemon still has no sessions to
			// recognise its label by. That address becomes valid the moment restore
			// finishes, so this must be the RETRYING notice — the expired page never
			// re-requests, and the frame would sit on it until a manual reload even
			// though the daemon recovered seconds later.
			if p.previewWarmingUp != nil && p.previewWarmingUp() {
				writeTabNoticePage(w, "Starting up",
					"af is starting up — this tab will load as soon as the daemon has restored its sessions.", true)
				return
			}
			// Otherwise the address really is dead: a web tab's label does not survive
			// the secret rotating, and an editor label the restored daemon cannot place
			// names no session it holds. Deliberately NON-retrying — nothing will make
			// it valid — and the web client mints a fresh origin when it rebuilds.
			writePreviewExpiredPage(w)
			return
		}
		writeHTTPError(w, r, http.StatusUnauthorized, errUnauthorized)
		return
	}
	next.ServeHTTP(w, r)
}

// withAuth wraps the daemon mux with a STATIC posture — the gate and CORS list are
// fixed at construction. Used by the trusted unix socket (nil gate) and the
// agent-server (fixed strict gate). The daemon's own web + preview listeners use
// withLivePosture instead so #2480 config changes apply with no rebind.
func withAuth(next http.Handler, gate *authGate, corsOrigins []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePosture(w, r, next, requestPosture{gate: gate, cors: corsOrigins})
	})
}

// withLivePosture wraps next so the auth + CORS posture is read from live config
// ONCE per request (#2480 PR2), making require_token / require_loopback_token /
// cors_allowed_origins take effect on the NEXT request with no socket rebind. This
// is deliberately DECOUPLED from the listener rebind: a tightening (require_token
// = true) applies to the next request whether or not any concurrent listen_addr
// rebind succeeds, so a security change can never fail in the permissive direction
// because a socket operation failed. buildPosture must read its config snapshot
// exactly once and derive the whole returned posture from it.
func withLivePosture(next http.Handler, buildPosture func() requestPosture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePosture(w, r, next, buildPosture())
	})
}

// writeAuthInfo answers GET /v1/auth-info with whether this peer must present a
// token. A nil gate (the trusted unix socket) never requires one. The predicate
// is computed from the transport RemoteAddr and the gate policy only — the same
// source of truth the enforcement path uses.
func writeAuthInfo(w http.ResponseWriter, r *http.Request, gate *authGate) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, r, http.StatusMethodNotAllowed,
			fmt.Errorf("method %s not allowed for %q; use GET", r.Method, authInfoPath))
		return
	}
	required := gate != nil && gate.authRequired(r)
	writeHTTPSuccess(w, r, authInfoResponse{AuthRequired: required})
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
	// DELETE is advertised for /v1/config-assistant (#2467), the first non-GET/POST
	// route: without it a cross-origin client from a configured cors_origins entry
	// gets a preflight that omits DELETE and the browser blocks the reap — the only
	// escape hatch from an assistant left running. Harmless for the same-origin SPA
	// default (which does no preflight), which is why nothing else surfaced it.
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
