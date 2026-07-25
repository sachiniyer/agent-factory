package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The web-tab PREVIEW origin's auth (#1856 steps 2-3).
//
// The preview listener (preview_listen_addr) is a SEPARATE origin from the SPA. It
// authenticates a PER-TAB credential, NOT the daemon bearer: the daemon holds an
// in-memory HMAC secret (Manager.previewSecret) and a tab's token is
// previewTabToken(secret, sessionID, tabID). The listener's gate DERIVES the expected
// token from the sid/tid in the request path and compares (previewExpectedToken), and
// the token is delivered under the #2400 clean-before-render discipline on distinct
// query/cookie NAMES from the daemon bearer.
//
// STEP 3a — the per-tab credential (this file). Step 2 shipped ONE process-global
// preview token in a cookie scoped to the whole webtabPathPrefix; that was acceptable
// only because the mux served nothing, and it must not survive to the moment content
// is served (a global token on a served origin is a cross-session read — worse than
// the status quo; see #1856). This step replaces it: the gate expects
// previewTabToken(secret, sid, tid) for the request's OWN tab, so presenting tab A's
// token to tab B's route is 401; the bootstrap cookie is scoped to
// /v1/webtab/<sid>/<tid>/, not the whole prefix; and /v1/preview-auth requires a
// session/tab selector and returns only that tab's token. The secret itself is never
// vended or logged.
//
// Why the credential is necessary but not SUFFICIENT for cross-tab isolation, stated
// precisely because the naive framing is wrong: cookies are scoped by (host, path)
// and NOT by port (RFC 6265 §8.5). On the CURRENT single shared preview origin, a
// page for tab A doing a same-origin fetch of tab B's path still has B's path-scoped
// cookie ambiently attached (attachment is by destination path, not initiator), so
// the credential alone cannot stop a cross-tab READ. That read is closed by giving
// each tab a distinct ORIGIN (CORS then blocks the cross-origin read) — the step 3b
// work (subdomain-per-tab, sandbox relax, client re-addressing). The per-tab
// credential here is what makes that safe: it authenticates the tab, so a leaked or
// observed token for one tab never authorizes another. Both land before any content
// is served or the sandbox relaxes (#1856 gate).
//
// The proxy must still strip BOTH af credential names in both directions
// (forwardAppCookies / the upstream query strip in webtab_proxy.go), because the two
// origins share one cookie jar and the untrusted dev server must receive neither.
//
// This step still serves NO content: newPreviewMux 404s after the bootstrap. Step 3b
// wires the per-tab origin + proxy routes.

// previewTokenCookie and previewTokenQueryParam carry the PREVIEW credential on
// the preview origin. They are deliberately distinct from webtabTokenCookie /
// webtabTokenQueryParam: the value is the preview token, never the daemon bearer,
// and a distinct name keeps a request that somehow carried both from ever letting
// one credential be read as the other. Same string for cookie and query because
// it is the same credential on two transports (mirrors the webtab pair).
const previewTokenCookie = "af_preview_token"     //nolint:gosec // cookie name, not a credential
const previewTokenQueryParam = "af_preview_token" //nolint:gosec // query-param name, not a credential

// previewPresentedToken extracts the credential a preview-origin request presents:
// the Authorization header first (a direct client), then the private query param
// (the iframe's top-level navigation, which cannot set a header), then the scoped
// cookie (sub-resource requests, set by the clean-before-render bootstrap). It is
// the preview listener's gate extractor (authGate.presentedToken), so the preview
// origin authenticates ONLY the preview token and never ?access_token= or the
// af_webtab_token transport.
func previewPresentedToken(r *http.Request) string {
	if tok := agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader)); tok != "" {
		return tok
	}
	if r.URL == nil {
		return ""
	}
	if tok := r.URL.Query().Get(previewTokenQueryParam); tok != "" {
		return tok
	}
	if c, err := r.Cookie(previewTokenCookie); err == nil {
		return c.Value
	}
	return ""
}

// previewTabToken derives the preview credential for one tab: the base64url
// HMAC-SHA256 of "<sessionID>\x00<tabID>" under secret. Session and tab ids cannot
// contain a NUL byte, so the separator makes the (sid, tid) pair unambiguous — no
// concatenation collision can let one tab's token authenticate another. The secret
// is the in-memory Manager.previewSecret; the derivation is deterministic, so the
// gate (which knows the secret and the request's sid/tid) and /v1/preview-auth
// (which mints for a requested sid/tid) agree without any shared per-tab state.
func previewTabToken(secret, sessionID, tabID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sessionID))
	mac.Write([]byte{0})
	mac.Write([]byte(tabID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parsePreviewTabPath extracts (sessionID, tabID) from a preview request's ESCAPED
// path "/v1/webtab/<sid>/<tid>/…", unescaping each segment exactly once. It takes the
// escaped path (r.URL.EscapedPath()) rather than the pre-decoded r.URL.Path so it
// derives the SAME ids the mux's PathValue and /v1/preview-auth see — a segment with
// a percent-encoded byte decodes to one id, not two path segments. ok=false for any
// path that is not a two-segment tab route (a bare prefix, a missing tab, an unescape
// error), which the gate treats as "no expected token" — a fail-closed deny.
func parsePreviewTabPath(escapedPath string) (sessionID, tabID string, ok bool) {
	rest, found := strings.CutPrefix(escapedPath, webtabPathPrefix)
	if !found {
		return "", "", false
	}
	segs := strings.SplitN(rest, "/", 3)
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return "", "", false
	}
	sid, err := url.PathUnescape(segs[0])
	if err != nil {
		return "", "", false
	}
	tid, err := url.PathUnescape(segs[1])
	if err != nil {
		return "", "", false
	}
	return sid, tid, true
}

// previewExpectedToken is the preview listener's request-aware expected-token
// function (authGate.expectedTokenForRequest). It derives the token the request's
// OWN tab must present, so the gate authenticates PER TAB: a request to tab B's route
// is compared only against tab B's token, and tab A's token fails closed. A path that
// names no tab yields an empty expected token, which ConstantTimeEqual rejects.
func previewExpectedToken(m *Manager) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		if r.URL == nil {
			return "", nil
		}
		sid, tid, ok := parsePreviewTabPath(r.URL.EscapedPath())
		if !ok {
			return "", nil
		}
		return previewTabToken(m.previewSecret, sid, tid), nil
	}
}

// previewListenerAuth is the credential wiring the preview listener's gate uses
// instead of the daemon token file: a PER-TAB expected token derived from the sid/tid
// in the request path (previewExpectedToken), and previewPresentedToken for the
// request side. There is no single token to advertise — per-tab tokens are vended via
// /v1/preview-auth.
func previewListenerAuth(m *Manager) *tcpListenerAuth {
	return &tcpListenerAuth{
		expectedForRequest: previewExpectedToken(m),
		presented:          previewPresentedToken,
	}
}

// newPreviewMux is the handler mounted on the preview listener (#1856). It runs
// the clean-before-render bootstrap for a preview-origin navigation and otherwise
// serves NOTHING yet — the preview proxy routes land in step 3.
//
// It is reached only AFTER withAuth's preview gate authorized the request against
// the request's OWN tab token, so the bootstrap here only moves that already-validated
// credential from the URL into the cookie and redirects to the clean URL. The cookie
// is scoped to /v1/webtab/<sid>/<tid>/ — this tab only — so tab A's token cookie is
// never sent on a request to tab B's path (the per-tab narrowing #1856 requires
// before content is served). It is still safe that content is not served yet; step 3b
// wires the proxy.
func newPreviewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(webtabPathPrefix+"{sessionId}/{tabId}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		// The gate already authenticated the request against this tab, so the sid/tid
		// are trusted; scope the credential cookie to exactly this tab's path (trailing
		// slash so it never prefix-matches a sibling tab whose id shares this prefix).
		tabCookiePath := webtabPathPrefix + r.PathValue("sessionId") + "/" + r.PathValue("tabId") + "/"
		if cleanBootstrapToken(w, r, previewTokenQueryParam, previewTokenCookie, tabCookiePath) {
			return
		}
		// Authenticated, credential parked in the cookie — but there is nothing to
		// serve yet. Step 3b replaces this with the preview proxy.
		writeHTTPError(w, r, http.StatusNotFound, errors.New("web-tab preview content is not wired yet (#1856 step 3b)"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusNotFound, fmt.Errorf("unknown preview route %q", r.URL.Path))
	})
	return mux
}

// previewAuthResponse is the body of GET /v1/preview-auth?session=&tab=: the preview
// origin's ephemeral bearer token FOR THAT TAB, which an authenticated control-plane
// client (the SPA) then delivers to that tab's preview iframe. Empty token ⇒ the
// preview listener is not bound (disabled, or its bind failed), so there is nothing
// to authenticate.
type previewAuthResponse struct {
	Token string `json:"token"`
}

// previewAuthHandler answers GET /v1/preview-auth?session=<sid>&tab=<tid> on the
// CONTROL listener with the PER-TAB preview token. It is behind the daemon's normal
// auth gate, so only a client already authorized to the control plane can read it —
// which is fine, that client can already do everything, and the token it learns is
// strictly LESS privileged than the daemon bearer: it authenticates ONE tab's preview
// origin, nothing else. It is the ONLY way to obtain a tab's token: the secret lives
// in memory, is never written to disk, and is never logged — only the per-tab
// derivation is returned, for the requested (session, tab).
//
// session and tab are REQUIRED; a missing selector is a 400 rather than a global
// token, so the endpoint can never hand back a credential that authenticates more
// than one tab. It does not check that the tab exists: the caller already holds the
// daemon bearer (it could enumerate tabs anyway), and a token for a nonexistent tab
// authenticates a route the proxy will 404 — no privilege is conferred.
//
// The response is no-store: it carries a raw bearer, so it must never sit in a
// shared/proxy cache (the recommended deployment fronts af with nginx/Caddy). This
// mirrors the clean-before-render bootstrap, which sets the same on its
// credential-bearing redirect.
//
// The token is withheld (empty) unless the preview listener is actually BOUND, not
// merely configured. The bind is deliberately non-fatal (a port conflict leaves
// PreviewConfigured=true, PreviewBound=false), and vending a live-looking token for
// a dead port would only mislead the client into addressing an origin that will not
// answer.
func (cs *controlServer) previewAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, r, http.StatusMethodNotAllowed,
			fmt.Errorf("method %s not allowed for /v1/preview-auth; use GET", r.Method))
		return
	}
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, errors.New("daemon has no session manager"))
		return
	}
	sid := r.URL.Query().Get("session")
	tid := r.URL.Query().Get("tab")
	if sid == "" || tid == "" {
		writeHTTPError(w, r, http.StatusBadRequest,
			errors.New("/v1/preview-auth requires non-empty session and tab query params"))
		return
	}
	token := ""
	if cs.manager.lifecycle != nil && cs.manager.lifecycle.snapshot().listeners.PreviewBound {
		token = previewTabToken(cs.manager.previewSecret, sid, tid)
	}
	// A raw credential in the body: keep it out of any shared cache.
	w.Header().Set("Cache-Control", "no-store")
	writeHTTPSuccess(w, r, previewAuthResponse{Token: token})
}
