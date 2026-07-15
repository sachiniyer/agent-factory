package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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

// WebTabTarget resolves the target URL of the web tab addressed by tabID — the
// tab's STABLE id (#1738), not its ordinal — in the session addressed by
// sessionID. It errors when the session or tab is missing, when the session is
// archived, or when the tab is not a web tab. The returned URL is the normalized
// target stored at create time.
//
// Addressing by id is what keeps an open preview pinned to the dev server it was
// opened on: closing a LOWER tab shifts every higher ordinal down, so an
// ordinal-keyed proxy would silently start relaying a DIFFERENT tab's dev server
// to a frame that never navigated (#1810). An id that names no live tab resolves
// to nothing — a clean 404 — rather than to whatever now occupies its old slot.
func (m *Manager) WebTabTarget(sessionID, tabID string) (string, error) {
	instance, _, _, err := m.resolveStreamSession(sessionID, "")
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("session %q not found", sessionID)
	}
	// An archived session is INERT, so its preserved web tab must not be served
	// (#1809 follow-up). Archive keeps the tab's URL so a restore can render it
	// again, but the target is a bare loopback address captured whenever the tab was
	// created: the dev server behind it is long gone, and the port may now host
	// something else entirely. Proxying it would make an archived session reach into
	// a live port on the daemon's machine — the opposite of inert. The tab starts
	// resolving again the moment a restore flips liveness back.
	//
	// Checked before the tab lookup: it is a property of the SESSION, so it holds
	// however the tab is addressed.
	if instance.IsArchived() {
		return "", fmt.Errorf("cannot open the web tab of archived session %q: it is inert until restored (af sessions restore)", sessionID)
	}
	idx, ok := instance.TabIndexByID(tabID)
	if !ok {
		return "", fmt.Errorf("session %q has no tab with id %q (it may have been closed)", sessionID, tabID)
	}
	tabs := instance.GetTabs()
	if idx < 0 || idx >= len(tabs) {
		// TabIndexByID resolved against a tab list that changed under us.
		return "", fmt.Errorf("session %q has no tab with id %q (it may have been closed)", sessionID, tabID)
	}
	tab := tabs[idx]
	if tab.Kind != session.TabKindWeb {
		return "", fmt.Errorf("tab %q of session %q is not a web tab", tabID, sessionID)
	}
	if strings.TrimSpace(tab.URL) == "" {
		return "", fmt.Errorf("web tab %q of session %q has no target URL", tabID, sessionID)
	}
	return tab.URL, nil
}

// webTabProxyHandler reverse-proxies GET /v1/webtab/{sessionId}/{tabId}/{rest...}
// to the tab's loopback dev-server target ON THE DAEMON MACHINE. This is what
// makes a localhost dev-server preview visible to a REMOTE web-UI viewer (over
// Tailscale/SSH): the browser fetches this same-origin daemon path, the daemon
// (which shares the machine with the dev server) fetches the loopback target and
// relays it back. Same-origin also sidesteps the dev server's X-Frame-Options.
//
// THE URL MODEL — the browser-visible path MIRRORS the upstream path. The client
// mints the iframe src with the target's OWN path appended to the tab prefix
// (web/src/tabaddr.ts webProxyPath), so
//
//	target   http://localhost:3000/app/viewer.html
//	iframe   /v1/webtab/<sid>/<tabId>/app/viewer.html
//	upstream /app/viewer.html
//
// and this handler simply strips the prefix and forwards {rest...} VERBATIM. A
// bare request to the tab root redirects to the target's path so the mirror holds
// from the first navigation on (see mirrorRootRedirect).
//
// Mirroring the path — rather than re-resolving the remainder against the target —
// is what makes the whole class of sub-path bugs disappear, because the browser's
// own URL resolution now happens at the SAME DEPTH as the dev server's:
//
//   - a sibling link (x.css on /app/viewer.html) resolves to /v1/webtab/<sid>/<t>/app/x.css
//     → upstream /app/x.css;
//   - a PARENT-relative link (../shared.css) resolves to /v1/webtab/<sid>/<t>/shared.css
//     → upstream /shared.css — depth is preserved, so it cannot climb out of the prefix;
//   - a Set-Cookie Path=/app re-scopes by pure PREFIX-PREPEND to
//     /v1/webtab/<sid>/<t>/app, which is exactly the browser path those cookies must
//     ride on;
//   - a subdirectory target (/app/viewer.html) works outright.
//
// This REPLACES the document-resolution rule of #1806 (resolveUpstreamPath) and
// retires the subdirectory-target limits that PR documented as known.
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
	tabID := r.PathValue("tabId")
	rest := r.PathValue("rest")
	// Defense in depth: the stdlib ServeMux already cleans "." / ".." out of the
	// path before matching, but reject any residue so a crafted request can never
	// escape the proxied prefix.
	if strings.Contains(rest, "..") {
		writeHTTPError(w, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}

	// Addressed by the tab's STABLE id: a stale id (its tab was closed) is a clean
	// 404 here, never a silent bind to whatever tab took its old ordinal (#1810).
	target, err := cs.manager.WebTabTarget(sessionID, tabID)
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
	// own /v1/webtab/ token cookie or leaking to a sibling tab. Because the browser
	// path mirrors the upstream path, this is a pure prefix-prepend and the
	// re-scoped cookie lands on exactly the requests the app scoped it to.
	tabPathPrefix := webtabPathPrefix + sessionID + "/" + tabID

	// Keep the browser-visible URL mirroring the upstream one: a bare hit on the
	// tab root is sent to the target's own path, after which every relative URL the
	// app emits resolves at the right depth on its own.
	if rest == "" {
		if dest, ok := mirrorRootRedirect(tabPathPrefix, targetURL, r.URL.RawQuery); ok {
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = targetURL.Host
			// The browser path mirrors the upstream path, so the remainder under
			// the tab prefix IS the upstream path: forward it verbatim. rest
			// arrives percent-DECODED from the "{rest...}" wildcard and is assigned
			// as Path (not parsed), so net/url re-encodes it canonically and a
			// literal "?"/"#"/"%" in a filename cannot be misread as a
			// query/fragment/escape.
			pr.Out.URL.Path = "/" + strings.TrimLeft(rest, "/")
			pr.Out.URL.RawPath = ""
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
			rewriteSetCookiePaths(resp.Header, tabPathPrefix)
			// Send the app's own redirects back through the prefix rather than out
			// to the daemon's origin, which is where a bare "/login" would otherwise
			// land (#1843).
			rewriteRedirectLocation(resp, tabPathPrefix, targetURL)
			rewriteRefreshURL(resp.Header, tabPathPrefix, targetURL)
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

// rewriteRedirectLocation sends an upstream redirect back through this tab's
// proxy prefix, so the browser follows it to the proxied app rather than to the
// daemon's own origin (#1843). A dev app that 302s to "/login" would otherwise
// navigate the iframe to the daemon's /login — a 404 — breaking every login and
// post-action redirect flow, which is the primary reason the proxy exists.
//
// Only 3xx Location is rewritten. On a redirect the header is NAVIGATIONAL: the
// browser follows it, so it must name a browser-reachable path. On a 2xx (201
// Created, say) it instead IDENTIFIES a resource — the app's own JS may compare it
// against a canonical id it already holds, and prefixing it would corrupt that
// comparison for no navigational gain.
func rewriteRedirectLocation(resp *http.Response, prefix string, target *url.URL) {
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return
	}
	loc := resp.Header.Get("Location")
	if loc == "" { // 304, or a 3xx that carries none
		return
	}
	if dest, ok := rewriteUpstreamRef(loc, prefix, target); ok {
		resp.Header.Set("Location", dest)
	}
}

// rewriteRefreshURL rewrites the url= of a Refresh header ("5; url=/login"), which
// some dev apps and frameworks use as a delayed redirect. It escapes the prefix
// exactly the way a Location does, and it is the same rewrite, so it is fixed
// alongside. Refresh is not status-gated: it is a meta-refresh equivalent and
// normally rides a 200.
//
// A Refresh without a url= re-fetches the current URL, which is already correct
// under the prefix, so it is left alone.
func rewriteRefreshURL(h http.Header, prefix string, target *url.URL) {
	v := h.Get("Refresh")
	if v == "" {
		return
	}
	delay, rest, ok := strings.Cut(v, ";")
	if !ok {
		return
	}
	const urlKey = "url="
	trimmed := strings.TrimSpace(rest)
	if len(trimmed) < len(urlKey) || !strings.EqualFold(trimmed[:len(urlKey)], urlKey) {
		return
	}
	raw := strings.TrimSpace(trimmed[len(urlKey):])
	// The value may be quoted; keep whichever quoting the app chose.
	quote := ""
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		quote = string(raw[0])
		raw = raw[1 : len(raw)-1]
	}
	dest, ok := rewriteUpstreamRef(raw, prefix, target)
	if !ok {
		return
	}
	h.Set("Refresh", strings.TrimSpace(delay)+"; url="+quote+dest+quote)
}

// rewriteUpstreamRef maps a URL reference the upstream app emitted into this tab's
// proxy prefix, reporting false when the reference must be passed through
// untouched. It is the shared rule behind Location and Refresh.
//
// Because the browser path MIRRORS the upstream path, the mapping is the same pure
// prefix-prepend that re-scopes cookies:
//
//	/app/                      -> /v1/webtab/s/t/app/          (absolute path: prepend)
//	http://localhost:3000/app/ -> /v1/webtab/s/t/app/          (same upstream: strip origin, keep path)
//	app/x                      -> unchanged                    (relative: already at mirrored depth)
//	https://example.com/x      -> unchanged                    (foreign host)
//
// A RELATIVE reference needs no help: the browser resolves it against the current
// proxied URL, which sits at the same depth as the upstream one, so it lands on the
// right path by construction — the same property that makes relative sub-resource
// links work.
//
// A FOREIGN host is passed through verbatim: it is a real off-site redirect (an
// OAuth provider, say) that must leave the frame. Rewriting it would both point the
// browser at a prefix the daemon would then refuse to proxy (only loopback targets
// are proxied) and silently rehost someone else's origin under ours.
//
// The path is carried VERBATIM, in the upstream's own encoding: Path and RawPath are
// prefixed together so url.String() reproduces the app's escaping rather than
// re-canonicalizing it (a literal %2F in a redirect target stays %2F).
func rewriteUpstreamRef(ref, prefix string, target *url.URL) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", false // unparseable: pass through rather than mangle
	}
	switch {
	case u.Scheme != "" || u.Host != "":
		// Absolute, or protocol-relative (//host/path). Ours to rewrite only if it
		// names the very server we proxy; anything else (including mailto:/data:,
		// which carry no host) leaves untouched.
		if !sameUpstreamHost(u, target) {
			return "", false
		}
		u.Scheme, u.Host, u.User = "", "", nil
	case !strings.HasPrefix(u.Path, "/"):
		return "", false // relative — already mirrored
	}
	if u.Path == "" { // origin-only, e.g. http://localhost:3000?x=1
		u.Path = "/"
	}
	escaped := u.EscapedPath()
	u.Path = joinURLPath(prefix, u.Path)
	u.RawPath = joinURLPath(prefix, escaped)
	return u.String(), true // query and fragment ride along untouched
}

// sameUpstreamHost reports whether ref names the same origin as the tab's proxied
// target, and so is a self-redirect the proxy should keep inside its prefix.
//
// Loopback ALIASES are deliberately not treated as equal: a target of
// localhost:3000 and a redirect to 127.0.0.1:3000 are the same server in almost
// every setup, but "almost" is what #1810 already paid for here. Binding a frame to
// a server it never named is the failure this file exists to prevent, so an alias
// mismatch degrades to an honest un-rewritten redirect instead. The realistic case
// costs nothing: the Rewrite hook sends the upstream its own Host, so a
// self-redirect echoes the target's host string verbatim.
//
// Scheme must match too. An http->https self-redirect is the app upgrading an origin
// the proxy does not speak; rewriting it would strip the upgrade and hand the
// request straight back to the http upstream, which would redirect again — an
// infinite loop in the frame rather than a visible failure.
func sameUpstreamHost(ref, target *url.URL) bool {
	if ref.Host == "" {
		return false
	}
	scheme := ref.Scheme
	if scheme == "" {
		scheme = target.Scheme // protocol-relative inherits the upstream hop's scheme
	}
	if !strings.EqualFold(scheme, target.Scheme) {
		return false
	}
	return normalizedHostPort(ref.Host, scheme) == normalizedHostPort(target.Host, target.Scheme)
}

// normalizedHostPort renders host:port lowercased with the scheme's default port
// made explicit, so "localhost" and "localhost:80" compare equal under http.
func normalizedHostPort(host, scheme string) string {
	h := strings.ToLower(host)
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h // already carries a port ("[::1]:3000" included)
	}
	switch strings.ToLower(scheme) {
	case "http":
		return h + ":80"
	case "https":
		return h + ":443"
	}
	return h
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

// requestIsHTTPS reports whether the request reached the daemon over TLS, so the
// web-tab token cookie can carry Secure exactly when the browser will accept it.
//
// The daemon's own listener serves PLAIN HTTP by design (tcpserver.go — the
// HTTP-only migration removed TLS), and a browser SILENTLY DROPS a Secure cookie
// delivered over http:// to a non-localhost origin. Flagging it unconditionally
// therefore killed the cookie in the one deployment it exists for — a network
// (Tailscale/SSH) peer with require_token=true — so every iframe sub-resource
// 401'd and the preview rendered unstyled (#1808). Loopback hid the bug: Chrome
// treats http://127.0.0.1 as a secure context AND loopback peers are token-exempt.
//
// r.TLS covers a future direct-TLS listener; X-Forwarded-Proto covers the
// RECOMMENDED deployment, where a front proxy terminates TLS and speaks plain HTTP
// to the daemon — there the cookie both can and should be Secure. The header is
// only trusted to ADD the flag: a peer that forges it merely asks for a stricter
// cookie its own plain-HTTP browser will then refuse to store, which fails closed
// (a broken preview) rather than open. It can never remove protection or
// authenticate anything — the token itself is still verified by the auth gate.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// A proxy chain may append (X-Forwarded-Proto: https, http); the FIRST entry is
	// the scheme the original client actually used.
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// mirrorRootRedirect computes where a bare request to a tab's proxy root should be
// sent so the browser-visible URL starts MIRRORING the target's path, and reports
// whether a redirect is needed at all.
//
//	prefix /v1/webtab/s/t + target /app/viewer.html -> /v1/webtab/s/t/app/viewer.html, true
//	prefix /v1/webtab/s/t + target /app/            -> /v1/webtab/s/t/app/,            true
//	prefix /v1/webtab/s/t + target /                -> "",                             false
//	prefix /v1/webtab/s/t + target "" (host-only)   -> "",                             false
//
// A root-URL target already mirrors itself, so it is left alone — redirecting it
// to its own path would be an infinite loop. Any other target redirects exactly
// once: the destination's remainder is non-empty, so the follow-up request takes
// the proxy path rather than returning here.
//
// The query (which carries ?access_token for a network peer's top-level
// navigation) rides along, so the redirected request authorizes even if the
// browser has not yet stored the token cookie.
func mirrorRootRedirect(prefix string, target *url.URL, rawQuery string) (string, bool) {
	if target.Path == "" || target.Path == "/" {
		return "", false
	}
	dest := &url.URL{
		Path:     prefix + "/" + strings.TrimLeft(target.Path, "/"),
		RawQuery: rawQuery,
	}
	return dest.String(), true
}

// joinURLPath joins a base path and a sub path with exactly one slash between
// them. Used to re-scope an upstream cookie's Path under the tab's proxy prefix.
// Because the browser path mirrors the upstream path, prepending the prefix is all
// a correct re-scope takes.
func joinURLPath(base, sub string) string {
	if base == "" || base == "/" {
		return sub
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}
