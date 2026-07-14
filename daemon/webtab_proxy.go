package daemon

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// webtabPathPrefix is the path all web-tab reverse-proxy requests sit under. It
// is the one route where the scoped af_webtab_token cookie is honored (see
// webTabAwareToken) so an iframe's sub-resource requests — which cannot carry the
// Authorization header or the ?access_token query — still authenticate.
const webtabPathPrefix = "/v1/webtab/"

// webtabTokenCookie carries the bearer token for web-tab sub-resource requests on
// the TCP/TLS listener. The proxy handler sets it (scoped to webtabPathPrefix)
// after a header/query token first authorized the iframe's top-level navigation;
// the auth gate then accepts it for FOLLOW-UP requests under that prefix only.
const webtabTokenCookie = "af_webtab_token" //nolint:gosec // cookie name, not a credential

// WebTabTarget resolves the target URL of the web tab at tabIdx in the session
// addressed by sessionID (the stable id the web client uses). It errors when the
// session or tab is missing, or when the tab is not a web tab. The returned URL
// is the normalized target stored at create time.
func (m *Manager) WebTabTarget(sessionID string, tabIdx int) (string, error) {
	instance, _, _, err := m.resolveStreamSession(sessionID, "")
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("session %q not found", sessionID)
	}
	tabs := instance.GetTabs()
	if tabIdx < 0 || tabIdx >= len(tabs) {
		return "", fmt.Errorf("session %q has no tab at index %d", sessionID, tabIdx)
	}
	tab := tabs[tabIdx]
	if tab.Kind != session.TabKindWeb {
		return "", fmt.Errorf("tab %d of session %q is not a web tab", tabIdx, sessionID)
	}
	if strings.TrimSpace(tab.URL) == "" {
		return "", fmt.Errorf("web tab %d of session %q has no target URL", tabIdx, sessionID)
	}
	return tab.URL, nil
}

// webTabProxyHandler reverse-proxies GET /v1/webtab/{sessionId}/{tabIdx}/{rest...}
// to the tab's loopback dev-server target ON THE DAEMON MACHINE. This is what
// makes a localhost dev-server preview visible to a REMOTE web-UI viewer (over
// Tailscale/SSH): the browser fetches this same-origin daemon path, the daemon
// (which shares the machine with the dev server) fetches the loopback target and
// relays it back. Same-origin also sidesteps the dev server's X-Frame-Options.
//
// It proxies ONLY loopback targets (localhost/127.0.0.1/::1); an external target
// is rejected here (it is iframed directly by the web UI, never routed through the
// daemon) so the daemon can never be turned into an open proxy / SSRF vector. The
// route is auth-gated by withAuth like the rest of the API, with the loopback
// exemption (#1697) honored and the webtabTokenCookie fallback for iframe
// sub-resource requests.
func (cs *controlServer) webTabProxyHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	sessionID := r.PathValue("sessionId")
	rest := r.PathValue("rest")
	tabIdx, err := strconv.Atoi(r.PathValue("tabIdx"))
	if err != nil || tabIdx < 0 {
		writeHTTPError(w, http.StatusBadRequest, fmt.Errorf("invalid web tab index %q", r.PathValue("tabIdx")))
		return
	}
	// Defense in depth: the stdlib ServeMux already cleans "." / ".." out of the
	// path before matching, but reject any residue so a crafted request can never
	// escape the proxied prefix.
	if strings.Contains(rest, "..") {
		writeHTTPError(w, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}

	target, err := cs.manager.WebTabTarget(sessionID, tabIdx)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound, err)
		return
	}
	// Only loopback targets are proxied. An external target must never be fetched
	// by the daemon (open-proxy / SSRF) — the web UI iframes those directly.
	if !session.IsLoopbackWebTarget(target) {
		writeHTTPError(w, http.StatusBadRequest,
			fmt.Errorf("web tab target %q is not loopback; external URLs are iframed directly, not proxied", target))
		return
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, fmt.Errorf("invalid web tab target: %w", err))
		return
	}

	// On the TCP/TLS listener a network peer authorized this top-level request via
	// the ?access_token query (an iframe src cannot set the Authorization header).
	// Persist that token as a path-scoped cookie so the framed app's sub-resource
	// GETs — which carry neither header nor query — stay authorized. Loopback peers
	// present no token and need none, so nothing is set for them.
	if tok := agentproto.TokenFromRequest(r); tok != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     webtabTokenCookie,
			Value:    tok,
			Path:     webtabPathPrefix,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = targetURL.Host
			pr.Out.URL.Path = joinURLPath(targetURL.Path, "/"+rest)
			// Strip the daemon credential from what we forward upstream: the dev
			// server must never see the daemon's bearer token or the auth cookie.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			if pr.Out.URL.Query().Has(agentproto.AccessTokenQueryParam) {
				q := pr.Out.URL.Query()
				q.Del(agentproto.AccessTokenQueryParam)
				pr.Out.URL.RawQuery = q.Encode()
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// The proxied preview is served same-origin as the SPA, so a dev server
			// that sends X-Frame-Options would block its own preview from framing.
			// Strip it (and the frame-ancestors CSP directive) so the loopback
			// preview always renders — this only affects the user's own dev server,
			// viewed through their own daemon.
			resp.Header.Del("X-Frame-Options")
			stripFrameAncestors(resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.WarningLog.Printf("web tab proxy to %s failed: %v", targetURL.Redacted(), err)
			writeHTTPError(w, http.StatusBadGateway,
				fmt.Errorf("web tab dev server at %s is unreachable: %w", targetURL.Host, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

// stripFrameAncestors removes the frame-ancestors directive from any
// Content-Security-Policy response headers, leaving the rest of each policy
// intact, so a dev server's CSP can't block its own same-origin preview from
// being framed. Other directives (script-src, etc.) are preserved verbatim.
func stripFrameAncestors(h http.Header) {
	values := h.Values("Content-Security-Policy")
	if len(values) == 0 {
		return
	}
	rewritten := make([]string, 0, len(values))
	for _, policy := range values {
		directives := strings.Split(policy, ";")
		kept := directives[:0]
		for _, d := range directives {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d)), "frame-ancestors") {
				continue
			}
			kept = append(kept, d)
		}
		rewritten = append(rewritten, strings.Join(kept, ";"))
	}
	h.Del("Content-Security-Policy")
	for _, p := range rewritten {
		if strings.TrimSpace(p) != "" {
			h.Add("Content-Security-Policy", p)
		}
	}
}

// joinURLPath joins a base path and a sub path with exactly one slash between
// them, so a target with its own base path ("/app") composes with the proxied
// remainder correctly.
func joinURLPath(base, sub string) string {
	if base == "" || base == "/" {
		return sub
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}
