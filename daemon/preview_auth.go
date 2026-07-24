package daemon

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The web-tab PREVIEW origin's auth (#1856 step 2).
//
// The preview listener (preview_listen_addr) is a SEPARATE origin from the SPA,
// so — once step 3 relaxes the sandbox — a framed dev server there cannot reach
// the SPA's storage or the daemon bearer. This file gives that origin its own
// credential: the daemon mints an ephemeral preview token (Manager.previewToken),
// the listener's gate compares against it, and it is delivered to the browser
// under the #2400 clean-before-render discipline exactly like the app-origin
// web-tab flow — but on distinct query/cookie names holding the distinct
// low-privilege credential, so the two can never be read for one another.
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
// The cookie is scoped to webtabPathPrefix: the preview origin serves its content
// under the same /v1/webtab/<sid>/<tid>/ path the app origin mirrors, so the
// browser sends the preview cookie on exactly those sub-resource requests.
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
// then delivers to the preview iframe. Empty token ⇒ the preview listener is
// disabled (preview_listen_addr unset), so there is nothing to authenticate.
type previewAuthResponse struct {
	Token string `json:"token"`
}

// previewAuthHandler answers GET /v1/preview-auth on the CONTROL listener with the
// ephemeral preview token. It is behind the daemon's normal auth gate, so only a
// client already authorized to the control plane can read it — which is fine, that
// client can already do everything, and the preview token it learns is strictly
// LESS privileged. It is the ONLY way to obtain the token: the token lives in
// memory, is never written to disk, and is never logged.
//
// When preview_listen_addr is unset the token is withheld (empty): there is no
// preview listener to present it to, so exposing an unusable credential would only
// mislead.
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
	if cs.manager.cfg != nil && cs.manager.cfg.PreviewListenAddr != "" {
		token = cs.manager.previewToken
	}
	writeHTTPSuccess(w, r, previewAuthResponse{Token: token})
}
