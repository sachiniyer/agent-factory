package daemon

import (
	"errors"
	"fmt"
	"net/http"
)

// The web-tab PREVIEW origin's credential (#1856 steps 2-3).
//
// The preview listener (preview_listen_addr) is a SEPARATE origin from the SPA, and
// it authenticates a PER-TAB credential rather than the daemon bearer: the daemon
// holds an in-memory HMAC secret (Manager.previewSecret) and a tab's credential is
// previewTabHostLabel(secret, sessionID, tabID) — the unguessable DNS label of that
// tab's own preview origin. The listener's gate re-derives the expected label from
// the tab the request's Host names and compares (previewOriginAuth), so a request
// whose Host names no minted tab is 401 and one tab's origin can never address
// another's. See preview_origin.go for the origin model itself.
//
// WHERE THE CREDENTIAL LIVES, and why it moved. Step 3a shipped the same per-tab
// HMAC on a cookie/query transport scoped to /v1/webtab/<sid>/<tid>/, which was
// correct while the preview mux served nothing. It cannot survive the per-tab
// origin: "localhost" is an effective TLD, so http://localhost:8443 (the SPA) and
// http://af….localhost:8444 (a tab) are different SITES, which makes every preview
// frame a third-party context. There, 3a's SameSite=Strict cookie is never sent,
// SameSite=None demands Secure on a plain-HTTP listener, and a browser with
// third-party cookies blocked withholds it anyway — every sub-resource would 401 and
// the preview would render blank. The Host header is the one channel the origin
// split cannot take away, so the capability rides it. Everything else from 3a is
// unchanged: the ephemeral secret, the per-tab derivation, the request-aware gate,
// and this endpoint as the ONLY way to learn a tab's address.
//
// The retired query/cookie NAMES are still stripped in both directions
// (forwardAppCookies, and the upstream query strip in webtab_serve.go). Nothing
// mints them any more, but a jar or a bookmark from an earlier release can still
// carry one, and the rule those strips implement is "cover everything any af gate
// has ever accepted" — the cost is a string comparison and the failure mode of
// getting it wrong is handing a credential to an untrusted dev server.

// previewTokenCookie and previewTokenQueryParam are the RETIRED preview-credential
// transports (#1856 step 3a). No af surface mints or accepts either any more — the
// per-tab credential is the origin's hostname — but both names are still scrubbed
// from anything travelling toward a previewed dev server, because the two origins
// share one cookie jar (cookies are scoped by host and path, NOT by port, RFC 6265
// §8.5) and an upgraded browser can still be holding one.
const previewTokenCookie = "af_preview_token"     //nolint:gosec // cookie name, not a credential
const previewTokenQueryParam = "af_preview_token" //nolint:gosec // query-param name, not a credential

// previewAuthResponse is the body of GET /v1/preview-auth?session=&tab=: the
// browser-facing ORIGIN of that tab's preview, e.g.
// "http://af<label>.localhost:8444". The origin IS the credential — its host label
// is an unguessable HMAC the preview listener's gate verifies — so there is no
// separate token field to hand out.
//
// An empty Origin means per-tab preview origins are unavailable: preview_listen_addr
// is unset, or its bind failed. The client treats that as "keep using the
// same-origin sandboxed mirror", which is the pre-#1856 behavior and the only thing
// a REMOTE viewer can use in any case.
type previewAuthResponse struct {
	Origin string `json:"origin"`
}

// previewAuthHandler answers GET /v1/preview-auth?session=<sid>&tab=<tid> on the
// CONTROL listener with that tab's preview origin, registering the origin so the
// preview listener will resolve it. It is behind the daemon's normal auth gate, so
// only a client already authorized to the control plane can mint one — which is
// fine, that client can already do everything, and what it learns is strictly LESS
// privileged than the daemon bearer: it addresses ONE tab's preview, nothing else.
// It is the only way to obtain a tab's origin: the secret lives in memory, is never
// written to disk, and is never logged — only the per-tab derivation is returned,
// for the requested (session, tab).
//
// session and tab are REQUIRED; a missing selector is a 400 rather than a
// tab-agnostic answer, so the endpoint can never hand back an address that resolves
// to more than one tab. It does not check that the tab exists: the caller already
// holds the daemon bearer (it could enumerate tabs anyway), and an origin for a
// nonexistent tab addresses a route the proxy will 404 — no privilege is conferred.
//
// The response is no-store: the origin embeds a capability, so it must never sit in
// a shared/proxy cache (the recommended deployment fronts af with nginx/Caddy).
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
	// A raw capability in the body: keep it out of any shared cache.
	w.Header().Set("Cache-Control", "no-store")
	writeHTTPSuccess(w, r, previewAuthResponse{Origin: previewOriginFor(cs.manager, sid, tid)})
}
