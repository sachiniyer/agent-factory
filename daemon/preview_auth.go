package daemon

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The web-tab PREVIEW origin's auth (#1856 step 2).
//
// The preview listener (preview_listen_addr) is a SEPARATE origin from the SPA
// (different port), so DOM/storage IS isolated: a framed dev server on the preview
// origin cannot read the SPA's window or localStorage. This file gives that origin
// its own low-privilege credential: the daemon mints an ephemeral preview token
// (Manager.previewToken), the listener's gate compares against it, and it is
// delivered under the #2400 clean-before-render discipline like the app-origin
// web-tab flow — on distinct query/cookie NAMES.
//
// What separation this actually buys, stated precisely because the naive framing
// is wrong: cookies are scoped by (host, path) and NOT by port (RFC 6265 §8.5), so
// the two origins share ONE cookie jar. Both credential cookies (af_webtab_token,
// af_preview_token) are transmitted to BOTH ports on every /v1/webtab/ request.
// The separation is enforced by the EXTRACTOR, not the browser: previewPresentedToken
// reads only af_preview_token and webTabAwareToken reads only af_webtab_token, so
// neither credential is HONORED on the other surface even though both are sent. The
// consequence for the proxy is that forwardAppCookies / the upstream query strip
// (webtab_proxy.go) MUST remove BOTH names, or the shared jar would leak the preview
// token to the untrusted dev server — see those sites.
//
// WHAT THIS STEP DELIBERATELY DOES NOT BUY — session isolation. Manager.previewToken
// is ONE process-global token, and the bootstrap parks it in a cookie scoped to the
// WHOLE webtabPathPrefix. So the credential a preview page for session A would carry
// is byte-identical to session B's, and is ambiently attached to a same-origin
// fetch of ANY session's preview path. That is acceptable ONLY because this step
// serves NO content: nothing sits behind the gate to read cross-session. It must NOT
// survive into step 3. Before the preview proxy serves a single byte, the credential
// has to become PER-TAB (an HMAC(secret, sid/tid)-derived token, gate validated
// against the sid/tid in the request path) with the cookie scoped to
// /v1/webtab/<sid>/<tid>/ — otherwise a global token turns the preview origin into a
// cross-session read and makes #1856 WORSE than the status quo. Even per-tab tokens
// only bound a page to ITS OWN credential; on this SHARED origin a concurrently-open
// other tab's path-scoped cookie is still ambiently attached to a cross-tab
// same-origin fetch, so FULL cross-tab isolation additionally needs per-tab ORIGINS
// (a listener/port per tab) — the origin-model decision step 3 must make, tracked on
// #1856. Do not read "separate origin" here as "sessions are isolated"; they are not
// yet.
//
// This step opens the auth handshake only; the preview origin still serves NO
// content (step 3 wires the proxy routes).

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

// previewListenerAuth is the token wiring the preview listener's gate uses instead
// of the daemon token file: the in-memory ephemeral preview token for both the
// advertised value and the per-auth-event comparison, and previewPresentedToken
// for the request side.
func previewListenerAuth(m *Manager) *tcpListenerAuth {
	return &tcpListenerAuth{
		token:     func() (string, error) { return m.previewToken, nil },
		presented: previewPresentedToken,
	}
}

// newPreviewMux is the handler mounted on the preview listener (#1856). It runs
// the clean-before-render bootstrap for a preview-origin navigation and otherwise
// serves NOTHING yet — the preview proxy routes land in step 3.
//
// It is reached only AFTER withAuth's preview gate authorized the request against
// the preview token, so the bootstrap here only moves that already-validated
// credential from the URL into the scoped cookie and redirects to the clean URL.
// The cookie is scoped to webtabPathPrefix — the WHOLE prefix, deliberately flagged:
// that is the non-isolating scope the file header warns must narrow to
// /v1/webtab/<sid>/<tid>/ before step 3 serves content. It is safe here only because
// nothing is served yet.
func newPreviewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(webtabPathPrefix+"{sessionId}/{tabId}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		if cleanBootstrapToken(w, r, previewTokenQueryParam, previewTokenCookie, webtabPathPrefix) {
			return
		}
		// Authenticated, credential parked in the cookie — but there is nothing to
		// serve yet. Step 3 replaces this with the preview proxy.
		writeHTTPError(w, r, http.StatusNotFound, errors.New("web-tab preview content is not wired yet (#1856 step 3)"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusNotFound, fmt.Errorf("unknown preview route %q", r.URL.Path))
	})
	return mux
}

// previewAuthResponse is the body of GET /v1/preview-auth: the preview origin's
// ephemeral bearer token, which an authenticated control-plane client (the SPA)
// then delivers to the preview iframe. Empty token ⇒ the preview listener is not
// bound (disabled, or its bind failed), so there is nothing to authenticate.
type previewAuthResponse struct {
	Token string `json:"token"`
}

// previewAuthHandler answers GET /v1/preview-auth on the CONTROL listener with the
// ephemeral preview token. It is behind the daemon's normal auth gate, so only a
// client already authorized to the control plane can read it — which is fine, that
// client can already do everything, and the preview token it learns is strictly
// LESS privileged than the daemon bearer (it authenticates only the preview origin,
// which serves nothing yet). It is NOT per-session: it is the one process-global
// token, so it does not distinguish sessions — the endpoint gains a session/tab
// selector when step 3 moves to per-tab tokens (see the file header). It is the ONLY
// way to obtain the token: the token lives in memory, is never written to disk, and
// is never logged.
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
	token := ""
	if cs.manager.lifecycle != nil && cs.manager.lifecycle.snapshot().listeners.PreviewBound {
		token = cs.manager.previewToken
	}
	// A raw credential in the body: keep it out of any shared cache.
	w.Header().Set("Cache-Control", "no-store")
	writeHTTPSuccess(w, r, previewAuthResponse{Token: token})
}
