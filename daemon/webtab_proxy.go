package daemon

import (
	"errors"
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
// the TCP listener. The proxy handler sets it (scoped to webtabPathPrefix)
// after a header/query token first authorized the iframe's top-level navigation;
// the auth gate then accepts it for FOLLOW-UP requests under that prefix only.
const webtabTokenCookie = "af_webtab_token" //nolint:gosec // cookie name, not a credential

// WebTabTarget resolves the loopback target of the iframe tab at tabIdx in the
// session addressed by sessionID (the stable id the web client uses). It errors
// when the session or tab is missing, or when the tab is not an iframe kind.
//
// For a web tab the target is the normalized URL stored at create time. For a
// VSCODE tab there is no stored URL by design: the target is the daemon-managed
// per-session code-server, ENSURED here — spawned on the first request and
// respawned if it died. Resolving on every request is what makes the editor
// self-heal (a crashed editor recovers on the next render or pane reload) and
// what makes restore work with no stored state: the port is chosen fresh each
// time, so a persisted URL would only ever be a stale port after a restart.
//
// A missing editor binary surfaces as errVSCodeBinaryMissing, which the proxy
// renders as an install hint rather than an error.
func (m *Manager) WebTabTarget(sessionID string, tabIdx int) (string, error) {
	instance, repoID, title, err := m.resolveStreamSession(sessionID, "")
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
	switch tab.Kind {
	case session.TabKindWeb:
		if strings.TrimSpace(tab.URL) == "" {
			return "", fmt.Errorf("web tab %d of session %q has no target URL", tabIdx, sessionID)
		}
		return tab.URL, nil
	case session.TabKindVSCode:
		return m.ensureVSCodeServer(instance, repoID, title)
	default:
		return "", fmt.Errorf("tab %d of session %q is not a web or vscode tab", tabIdx, sessionID)
	}
}

// ensureVSCodeServer returns the loopback base URL of the editor serving
// instance's worktree, starting it if needed. Keyed by daemonInstanceKey — the
// same key kill/archive stop it under — so every vscode tab and every pane in a
// session shares ONE editor.
func (m *Manager) ensureVSCodeServer(instance *session.Instance, repoID, title string) (string, error) {
	if m.vscode == nil {
		return "", fmt.Errorf("daemon has no VS Code supervisor")
	}
	worktree := instance.GetWorktreePath()
	if strings.TrimSpace(worktree) == "" {
		return "", fmt.Errorf("session %q has no worktree to open in VS Code", title)
	}
	return m.vscode.ensureServer(daemonInstanceKey(repoID, title), worktree)
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
		// A machine with no editor installed is an ordinary, actionable state, not
		// a failure: render the install hint INTO the pane (the iframe shows this
		// document) rather than an error page, and log nothing — this resolves on
		// every request, so an error log here would spam once per asset fetch.
		if errors.Is(err, errVSCodeBinaryMissing) {
			writeVSCodeNoticePage(w, vscodeInstallHint)
			return
		}
		// A cold code-server can outrun the start timeout on a slow machine. The
		// process is still coming up, so show a self-refreshing notice that turns
		// into the editor once it listens, rather than a dead error page the user
		// has to react to.
		if errors.Is(err, errVSCodeStarting) {
			writeVSCodeNoticePageRetry(w, "VS Code is still starting…", true)
			return
		}
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

	// On the TCP listener a network peer authorized this top-level request via
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
			Secure:   requestIsHTTPS(r),
			SameSite: http.SameSiteStrictMode,
		})
	}

	// The path prefix this tab's cookies are scoped under. Upstream Set-Cookie
	// paths are rewritten beneath it so a cookie-backed dev app (login/session/
	// CSRF) works in the iframe without its cookies colliding with the daemon's
	// own /v1/webtab/ token cookie or leaking to a sibling tab.
	cookiePathPrefix := webtabPathPrefix + sessionID + "/" + strconv.Itoa(tabIdx)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = targetURL.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = resolveUpstreamPath(targetURL, rest)
			// Never leak the daemon credential upstream: drop the Authorization
			// header and the daemon's own token cookie, but FORWARD the dev app's
			// cookies so cookie-backed dev servers work in the iframe.
			pr.Out.Header.Del("Authorization")
			forwardAppCookies(pr.Out)
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
			// Relay the dev app's Set-Cookie back to the browser, re-scoped under
			// this tab's proxy path (and Domain dropped so it defaults to the daemon
			// host) so the cookie lands on the right path and coexists with the
			// daemon's token cookie.
			rewriteSetCookiePaths(resp.Header, cookiePathPrefix)
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

// requestIsHTTPS reports whether the BROWSER's connection to the daemon is
// encrypted, which decides whether the web-tab token cookie may carry Secure.
//
// This has to be measured, not assumed. af terminates no TLS of its own (#1755
// removed it), so r.TLS is always nil and a hardcoded Secure:true was silently
// self-defeating: a browser REJECTS a Secure cookie set over a plain-HTTP origin,
// so on a network listener (a Tailscale IP, say) with require_token=true the
// cookie was never stored and every iframe sub-resource and WS upgrade — which is
// all a code-server is — came back 401. Loopback hid it: browsers treat
// http://localhost as a trustworthy origin and accept the cookie there.
//
// X-Forwarded-Proto covers the supported way to actually get HTTPS: a
// TLS-terminating reverse proxy in front of the daemon. Trusting a client-settable
// header here is safe because it can only ever HARM the sender — forging "https"
// onto a plain-HTTP request just marks their own cookie Secure so their own
// browser withholds it. It grants nothing: the auth gate never reads a header for
// its decisions (see isLoopbackRequest).
//
// Dropping Secure on a plain-HTTP connection concedes nothing either: that
// connection is already plaintext, and it already carried the same token in the
// ?access_token query. Transport security is the reverse proxy's or the VPN's job.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	// A proxy chain may append: take the first (the browser-facing) hop.
	if i := strings.Index(proto, ","); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// forwardAppCookies forwards the dev app's cookies upstream while stripping the
// daemon's own token cookie, so a cookie-backed dev server sees its session/CSRF
// cookies but never the daemon bearer token.
func forwardAppCookies(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	var b strings.Builder
	for _, c := range cookies {
		if c.Name == webtabTokenCookie {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteString("=")
		b.WriteString(c.Value)
	}
	if b.Len() > 0 {
		r.Header.Set("Cookie", b.String())
	}
}

// rewriteSetCookiePaths re-scopes the dev app's Set-Cookie headers under the
// tab's proxy path prefix so a cookie the app set for "/" (or "/api", …) lands on
// the proxied path the browser actually uses, coexisting with the daemon's
// /v1/webtab/ token cookie. Domain is dropped so the cookie defaults to the proxy
// (daemon) host rather than the dev server's own host. Unparseable Set-Cookie
// lines are passed through untouched.
func rewriteSetCookiePaths(h http.Header, prefix string) {
	values := h.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	rewritten := make([]string, 0, len(values))
	for _, line := range values {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			rewritten = append(rewritten, line)
			continue
		}
		orig := c.Path
		if orig == "" {
			orig = "/"
		}
		c.Path = joinURLPath(prefix, orig)
		c.Domain = "" // default to the proxy host
		if s := c.String(); s != "" {
			rewritten = append(rewritten, s)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range rewritten {
		h.Add("Set-Cookie", v)
	}
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

// resolveUpstreamPath computes the upstream path (and its escaped form) for a
// proxied web-tab request whose remainder under the tab's prefix is rest.
//
// The target is treated as a DOCUMENT reference and rest is resolved against it
// exactly as a browser resolves a link on that page (RFC 3986 §5.3):
//
//	target /viewer.html     + rest ""             -> /viewer.html
//	target /viewer.html     + rest "assets/x.css" -> /assets/x.css
//	target /app/viewer.html + rest "assets/x.css" -> /app/assets/x.css
//	target /app/            + rest "assets/x.css" -> /app/assets/x.css
//	target / (or "")        + rest "assets/x.css" -> /assets/x.css
//
// A root request therefore fetches the target path EXACTLY — appending a trailing
// slash to a file target ("/viewer.html/") makes a static file server (python
// -m http.server, and most dev servers) 404 the page the tab points at — while a
// relative sub-resource still lands next to the document it came from.
//
// rest arrives percent-DECODED from the ServeMux "{rest...}" wildcard, so it is
// assigned as the reference's Path rather than parsed with url.Parse: parsing
// would misread a literal "?" or "#" in a filename as a query/fragment, and a
// "//host"-style reference would try to swap the upstream host. Leading slashes
// are trimmed so rest is always relative to the target document.
func resolveUpstreamPath(target *url.URL, rest string) (path, rawPath string) {
	resolved := target.ResolveReference(&url.URL{Path: strings.TrimLeft(rest, "/")})
	if resolved.Path == "" {
		// Host-only target ("http://localhost:8899") fetched at its root.
		return "/", ""
	}
	return resolved.Path, resolved.RawPath
}

// joinURLPath joins a base path and a sub path with exactly one slash between
// them. Used to re-scope an upstream cookie's Path under the tab's proxy prefix,
// which is a true directory join (unlike the upstream URL, where the target is a
// document — see resolveUpstreamPath).
func joinURLPath(base, sub string) string {
	if base == "" || base == "/" {
		return sub
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}
