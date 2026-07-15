package daemon

import (
	"errors"
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

// WebTabTarget resolves the loopback target of the iframe tab addressed by tabID —
// the tab's STABLE id (#1738), not its ordinal — in the session addressed by
// sessionID. It errors when the session or tab is missing, when the session is
// archived, or when the tab is not an iframe kind. It also returns the tab's kind,
// which the proxy needs to shape the upstream request.
//
// Addressing by id is what keeps an open preview pinned to the dev server it was
// opened on: closing a LOWER tab shifts every higher ordinal down, so an
// ordinal-keyed proxy would silently start relaying a DIFFERENT tab's dev server
// to a frame that never navigated (#1810). An id that names no live tab resolves
// to nothing — a clean 404 — rather than to whatever now occupies its old slot.
// A VSCODE pane rides the same guarantee: a moved tab can never repoint it at
// another session's editor.
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
func (m *Manager) WebTabTarget(sessionID, tabID string) (string, session.TabKind, error) {
	instance, repoID, title, err := m.resolveStreamSession(sessionID, "")
	if err != nil {
		return "", 0, err
	}
	if instance == nil {
		return "", 0, fmt.Errorf("session %q not found", sessionID)
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
	//
	// It fences a VSCODE tab too, for a different reason with the same conclusion:
	// serving one SPAWNS an editor, and an archived session's worktree has been
	// moved out to the archive dir. (ensureVSCodeServer refuses archived sessions on
	// its own as well — this just refuses earlier, before any kind lookup.) The
	// message stays kind-agnostic because this runs before the kind is known.
	if instance.IsArchived() {
		return "", 0, fmt.Errorf("cannot open the tab of archived session %q: it is inert until restored (af sessions restore)", sessionID)
	}
	idx, ok := instance.TabIndexByID(tabID)
	if !ok {
		return "", 0, fmt.Errorf("session %q has no tab with id %q (it may have been closed)", sessionID, tabID)
	}
	tabs := instance.GetTabs()
	if idx < 0 || idx >= len(tabs) {
		// TabIndexByID resolved against a tab list that changed under us.
		return "", 0, fmt.Errorf("session %q has no tab with id %q (it may have been closed)", sessionID, tabID)
	}
	tab := tabs[idx]
	switch tab.Kind {
	case session.TabKindWeb:
		if strings.TrimSpace(tab.URL) == "" {
			return "", tab.Kind, fmt.Errorf("web tab %q of session %q has no target URL", tabID, sessionID)
		}
		return tab.URL, tab.Kind, nil
	case session.TabKindVSCode:
		target, err := m.ensureVSCodeServer(instance, repoID, title)
		return target, tab.Kind, err
	default:
		return "", tab.Kind, fmt.Errorf("tab %q of session %q is not a web or vscode tab", tabID, sessionID)
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
	// Never START an editor for a session that is archived or being torn down.
	// This route is NOT serialized with KillSession/ArchiveSession — it must not
	// be, since spawning blocks for seconds and would stall them — so without this
	// gate a stale iframe refresh (or simply selecting an archived row that still
	// has a vscode tab) could spawn an editor AFTER teardown already stopped one,
	// leaving a daemon-owned code-server rooted at a worktree that is being moved
	// or removed. TabSpawnBlocked is the same predicate CreateTab uses to refuse a
	// tab on an archived/mid-archive/mid-kill session: "may this session gain a
	// process right now" is exactly the question being asked here.
	//
	// It closes the archive window completely (BeginArchive raises the fence before
	// teardown) and most of the kill window; the deferred sweep in KillSession /
	// ArchiveSession catches anything that still races in, so the invariant holds
	// on timing rather than on luck.
	if err := instance.TabSpawnBlocked(); err != nil {
		return "", err
	}
	if instance.UserKilled() {
		return "", fmt.Errorf("session %q has been killed", title)
	}
	worktree := instance.GetWorktreePath()
	if strings.TrimSpace(worktree) == "" {
		return "", fmt.Errorf("session %q has no worktree to open in VS Code", title)
	}
	key := daemonInstanceKey(repoID, title)
	target, err := m.vscode.ensureServer(key, worktree)
	if err != nil {
		return "", err
	}
	// Re-check that a vscode tab still EXISTS, now that the spawn is done.
	//
	// CloseTab stops the editor under the op-lock this route deliberately does not
	// take, so the tab this request resolved can be closed — and its stopFor can
	// run — while the spawn is still in flight. The editor started here would then
	// belong to no tab: nothing renders it, and no close/archive/kill path for a
	// tab that no longer exists will ever stop it. Checking AFTER the spawn is what
	// closes that window; checking only before would leave it wide open for exactly
	// the seconds a spawn takes.
	if !instanceHasVSCodeTab(instance) {
		m.vscode.stopFor(key)
		return "", fmt.Errorf("the VS Code tab of session %q was closed", title)
	}
	return target, nil
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
	target, tabKind, err := cs.manager.WebTabTarget(sessionID, tabID)
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
			// Tell a VS Code editor which prefix the BROWSER reaches it under.
			//
			// The two editors differ here and it decides whether the fallback one
			// works at all. code-server emits RELATIVE URLs derived from the request
			// path's depth, so stripping the prefix is enough and this header is inert
			// to it. openvscode-server emits ABSOLUTE ones, and resolves its base from
			// X-Forwarded-Prefix — without it, its assets and WS point at the daemon's
			// ROOT rather than under /v1/webtab/..., and the editor never loads.
			//
			// Its --server-base-path flag is the documented alternative, but it cannot
			// be used here: it bakes ONE prefix into the process, while a single
			// per-SESSION editor is reached under a DIFFERENT prefix per tab index.
			// This header is per-request, so it composes with a shared editor.
			//
			// Set only for a vscode tab: for a web tab the target is an arbitrary dev
			// server, and a framework that honors this header would start rewriting its
			// URLs — a behavior change to today's previews that belongs in its own
			// change, not smuggled in here.
			if tabKind == session.TabKindVSCode {
				pr.Out.Header.Set("X-Forwarded-Prefix", tabPathPrefix)
			}
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
//	/../login                  -> /v1/webtab/s/t/login         (dot segments resolved first)
//	app/x                      -> unchanged                    (relative: already at mirrored depth)
//	https://example.com/x      -> unchanged                    (foreign host)
//	///example.com/x           -> unchanged                    (foreign host, network-path spelling)
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
// Everything here is decided the way the BROWSER will read the header, which is not
// always the way net/url parses it. Two spellings diverge, and both are handled
// before the prefix goes on: a network-path reference net/url hands back as a plain
// path (isNetworkPathRef), and dot segments that would otherwise eat the prefix
// after the browser normalizes them (normalizeEscapedPath).
//
// The path is carried VERBATIM, in the upstream's own encoding: the prefix is
// prepended to the ESCAPED path and the result re-parsed, so url.String() reproduces
// the app's escaping rather than re-canonicalizing it (a literal %2F in a redirect
// target stays %2F, leading or not).
func rewriteUpstreamRef(ref, prefix string, target *url.URL) (string, bool) {
	raw := strings.TrimSpace(ref)
	u, err := url.Parse(raw)
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
	case isNetworkPathRef(raw):
		// A foreign host in a spelling net/url reported as a local path. Same rule
		// as any other foreign host: pass it through untouched.
		return "", false
	}
	if u.Path == "" { // origin-only, e.g. http://localhost:3000?x=1
		u.Path = "/"
	}
	// Prefix the escaped form and re-parse it, rather than prefixing Path and RawPath
	// separately: the two must stay consistent or url.String() silently drops RawPath
	// and re-canonicalizes, and a re-parse is what net/url itself uses to keep them so.
	final := strings.TrimRight(prefix, "/") + normalizeEscapedPath(u.EscapedPath())
	if isNetworkPathRef(final) {
		// Unreachable for a real tab prefix (always "/v1/webtab/<sid>/<tab>"), which
		// is exactly why it is asserted rather than assumed: an empty prefix plus an
		// upstream "/..//evil.com" would otherwise emit a Location the browser reads
		// as an off-site host — this proxy handing out an open redirect.
		return "", false
	}
	p, err := url.Parse(final)
	if err != nil {
		return "", false
	}
	u.Path, u.RawPath = p.Path, p.RawPath
	return u.String(), true // query and fragment ride along untouched
}

// isAuthoritySlash reports whether c is a slash the browser's URL parser accepts as
// an authority delimiter for an http(s) URL. Backslash counts: the WHATWG parser
// folds "\" to "/" for these "special" schemes, so "/\host/x" reaches the same
// authority state "//host/x" does.
func isAuthoritySlash(c byte) bool { return c == '/' || c == '\\' }

// isNetworkPathRef reports whether a BROWSER would read ref as naming a HOST rather
// than a path on the current origin — RFC 3986's network-path reference, which the
// browser enters on a leading "//" and, for http(s), on any leading run of slashes
// or backslashes: "///example.com/x" and "/\example.com/x" both navigate to
// example.com.
//
// net/url recognizes only the exact two-slash spelling, filling in Host for it; the
// longer runs it hands back with an EMPTY host and the whole reference as Path. So
// an absolute-path test alone sees a local path and prefixes it, turning an upstream
// "Location: ///accounts.example.com/oauth" into a path on the dev server and
// stranding an OAuth handoff inside the frame.
//
// The test reads the RAW header value because that is the byte string the browser
// parses. A percent-escape is an ordinary path character to it, not a delimiter, so
// "/%2Ffoo" is deliberately NOT a network path here even though net/url decodes its
// Path to "//foo".
func isNetworkPathRef(ref string) bool {
	return len(ref) >= 2 && isAuthoritySlash(ref[0]) && isAuthoritySlash(ref[1])
}

// isSingleDotSegment and isDoubleDotSegment recognize dot segments in the forms a
// BROWSER resolves. The WHATWG URL parser decodes %2e before classifying a segment,
// so "%2e%2e" walks up exactly like ".." does. Neither url.ResolveReference nor
// path.Clean knows that — they compare the literal segment — which is why the rule
// is spelled out here instead of delegated.
func isSingleDotSegment(seg string) bool {
	return seg == "." || strings.EqualFold(seg, "%2e")
}

func isDoubleDotSegment(seg string) bool {
	return seg == ".." || strings.EqualFold(seg, ".%2e") ||
		strings.EqualFold(seg, "%2e.") || strings.EqualFold(seg, "%2e%2e")
}

// normalizeEscapedPath resolves the "." and ".." segments of an absolute escaped
// path, the way the browser resolves them when it follows the rewritten header.
//
// Doing it BEFORE the prefix goes on is what stops a dot segment from eating the
// prefix. An upstream "Location: /../login" prefixed verbatim yields
// "/v1/webtab/<sid>/<tab>/../login"; the browser normalizes that to
// "/v1/webtab/<sid>/login", which names a different tab — or, far more often, a 404
// — instead of the upstream's "/login". Normalizing first sends "/login" through
// the prefix, which is what the upstream meant and what an unproxied browser would
// have fetched.
//
// It walks the ESCAPED path, so a %2F stays an ordinary path character rather than
// becoming a segment separator.
func normalizeEscapedPath(escaped string) string {
	if !strings.HasPrefix(escaped, "/") {
		return escaped
	}
	segments := strings.Split(escaped[1:], "/")
	out := make([]string, 0, len(segments))
	for i, seg := range segments {
		last := i == len(segments)-1
		switch {
		case isSingleDotSegment(seg):
			if last {
				out = append(out, "") // "/a/." names the directory: keep its slash
			}
		case isDoubleDotSegment(seg):
			if len(out) > 0 {
				out = out[:len(out)-1] // at root already: ".." has nothing to pop
			}
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	return "/" + strings.Join(out, "/")
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
