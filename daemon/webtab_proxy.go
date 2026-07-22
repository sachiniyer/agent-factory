package daemon

import (
	"errors"
	"fmt"
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
// Authorization header or a query token — still authenticate.
const webtabPathPrefix = "/v1/webtab/"

// webtabTokenCookie carries the bearer token for web-tab sub-resource requests on
// the TCP listener. The proxy handler sets it (scoped to webtabPathPrefix)
// after a header/query token first authorized the iframe's top-level navigation;
// the auth gate then accepts it for FOLLOW-UP requests under that prefix only.
const webtabTokenCookie = "af_webtab_token" //nolint:gosec // cookie name, not a credential

// webtabTokenQueryParam is the query param the daemon's OWN credential rides on a
// web-tab iframe's top-level navigation. It is deliberately DISTINCT from the
// general agentproto.AccessTokenQueryParam ("access_token"): the proxy mirrors the
// framed target's WHOLE query to the dev server, so if the daemon also rode
// ?access_token= it would collide with a target that uses its own ?access_token=.
// The collision is not cosmetic — the daemon would read the app's value as its
// credential and 401 the iframe (auth reads the first value), and the exempt-peer
// strip would remove the app's value instead of the daemon's. A private name keeps
// the two credentials from ever meeting. Mirrored in web/src/tabaddr.ts
// (webProxyPath); shares the cookie's string because it is the same credential on
// a different transport.
const webtabTokenQueryParam = "af_webtab_token" //nolint:gosec // query-param name, not a credential

// webtabErrorHeader marks a proxy failure response the DAEMON generated, as opposed
// to one the dev server itself answered with (#1909).
//
// It exists because status alone cannot tell the two apart. The proxy relays upstream
// statuses unchanged, so an app that serves its own 502 — a framework proxy whose
// backend is down, a local gateway error page — is byte-for-byte indistinguishable
// from af's own "the dev server never answered" 502. The web client suppressed both
// and showed the dead-server fallback, hiding a page the app really served.
//
// The ErrorHandler REPLACES the response when the upstream never answered, and runs
// only then, so a marker set there is present exactly when af generated the failure.
// It is STRIPPED from every upstream response (see ModifyResponse) so the upstream
// cannot forge it: the client trusts this header, and what the client trusts the
// proxy must control (#1879).
const webtabErrorHeader = "X-AF-Webtab-Error"

// webtabErrorUpstreamUnreachable is the only reason af generates a failure for today:
// the transport never got an answer. The client keys on the header's PRESENCE, not
// this value, so a future reason needs no client change.
const webtabErrorUpstreamUnreachable = "upstream-unreachable"

// webTabTarget is where one iframe tab's traffic is sent. The two tab kinds reach
// their upstream over DIFFERENT transports, and this is what carries the
// difference to the proxy.
//
// A WEB tab names a real loopback dev server: URL is its address and the proxy
// dials TCP. A VSCODE tab is served over a unix socket the daemon owns (#1873):
// SocketPath is the real endpoint and URL is a dummy http://vscode.invalid that
// exists only because HTTP needs a host — the transport dials the socket and the
// host never reaches the wire.
type webTabTarget struct {
	// URL is the upstream base URL. Real for a web tab; a dummy for vscode.
	URL string
	// SocketPath, when non-empty, is the unix socket to dial INSTEAD of URL's
	// host. It is set only for a daemon-owned editor.
	SocketPath string
	// Transport dials SocketPath. It belongs to the editor process and outlives
	// the request, so it must be used rather than rebuilt here — see the field
	// comment on vscodeServer.transport for why per-request or shared would both
	// be wrong. Nil for a web tab, which leaves the proxy on http.DefaultTransport.
	Transport *http.Transport
	Kind      session.TabKind
}

// isUnixSocket reports whether this target is reached over a unix socket rather
// than TCP — the predicate that decides both the dialer and which safety check
// applies (loopback for TCP, the socket's own 0600-in-0700 perms for a socket).
func (t webTabTarget) isUnixSocket() bool { return t.SocketPath != "" }

// roundTripper is the proxy's transport for this target: the editor's socket
// dialer, or nil to leave httputil.ReverseProxy on http.DefaultTransport.
//
// It returns an INTERFACE, so a nil *http.Transport must become a nil
// RoundTripper explicitly — assigning a typed nil pointer to the interface field
// would make it non-nil, and ReverseProxy would call it and panic instead of
// falling back to the default.
func (t webTabTarget) roundTripper() http.RoundTripper {
	if t.Transport == nil {
		return nil
	}
	return t.Transport
}

// WebTabTarget resolves the target of the iframe tab addressed by tabID —
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
// what makes restore work with no stored state: the editor's socket is live only
// while its process is, so a persisted target could only ever be a dead endpoint
// after a restart.
//
// A missing editor binary surfaces as errVSCodeBinaryMissing, which the proxy
// renders as an install hint rather than an error.
func (m *Manager) WebTabTarget(sessionID, tabID string) (webTabTarget, error) {
	instance, repoID, title, err := m.resolveStreamSession(sessionID, "")
	if err != nil {
		return webTabTarget{}, err
	}
	if instance == nil {
		return webTabTarget{}, fmt.Errorf("session %q not found", sessionID)
	}
	// An archived session is INERT, so its preserved web tab must not be served
	// (#1809 follow-up). Archive keeps the tab's URL so a restore can render it
	// again, but the target is a bare loopback address captured whenever the tab was
	// created: the dev server behind it is long gone, and the port may now host
	// something else entirely. Proxying it would make an archived session reach into
	// a live port on the daemon's machine — the opposite of inert. The tab starts
	// resolving again the moment a restore begins (its worktree is home by then).
	//
	// WebTabServeBlocked, not the settled IsArchived: the fence has to go up when
	// the archive STARTS, not when it finishes. BeginArchive raises OpArchiving and
	// only then tears tmux down and moves the worktree, so a gate on LiveArchived
	// alone would let an already-open iframe keep proxying through that whole
	// teardown. This route is not serialized with ArchiveSession (a proxied request
	// must not block behind a worktree move), so the fence lives on the instance.
	//
	// Checked before the tab lookup: it is a property of the SESSION, so it holds
	// however the tab is addressed.
	//
	// It fences a VSCODE tab too, for a different reason with the same conclusion:
	// serving one SPAWNS an editor, and an archived session's worktree has been
	// moved out to the archive dir. (ensureVSCodeServer refuses on its own via
	// TabSpawnBlocked — this just refuses earlier, before any kind lookup.) The
	// message stays kind-agnostic because this runs before the kind is known.
	if blocked := instance.WebTabServeBlocked(); blocked != nil {
		return webTabTarget{}, fmt.Errorf("cannot open the tab of session %q: %w", sessionID, blocked)
	}
	// Resolved under ONE lock: id→ordinal then ordinal→tab takes the instance lock
	// twice, and a tab closing between the two shifts a LOWER ordinal away, leaving
	// the captured index in range but pointing at a different tab — which would
	// serve that tab's dev server under this id, the very misroute id-keying (#1810)
	// exists to prevent. A bounds check cannot see it; single-lock resolution can.
	kind, tabURL, ok := instance.TabTargetByID(tabID)
	if !ok {
		return webTabTarget{}, fmt.Errorf("session %q has no tab with id %q (it may have been closed)", sessionID, tabID)
	}
	switch kind {
	case session.TabKindWeb:
		if strings.TrimSpace(tabURL) == "" {
			return webTabTarget{Kind: kind}, fmt.Errorf("web tab %q of session %q has no target URL", tabID, sessionID)
		}
		return webTabTarget{URL: tabURL, Kind: kind}, nil
	case session.TabKindVSCode:
		endpoint, err := m.ensureVSCodeServer(instance, repoID, title)
		if err != nil {
			return webTabTarget{Kind: kind}, err
		}
		return webTabTarget{
			URL:        vscodeUpstreamURL,
			SocketPath: endpoint.SocketPath,
			Transport:  endpoint.Transport,
			Kind:       kind,
		}, nil
	default:
		return webTabTarget{Kind: kind}, fmt.Errorf("tab %q of session %q is not a web or vscode tab", tabID, sessionID)
	}
}

// ensureVSCodeServer returns the unix-socket endpoint of the editor serving
// instance's worktree, starting it if needed. Keyed by daemonInstanceKey — the
// same key kill/archive stop it under — so every vscode tab and every pane in a
// session shares ONE editor.
func (m *Manager) ensureVSCodeServer(instance *session.Instance, repoID, title string) (vscodeEndpoint, error) {
	if m.vscode == nil {
		return vscodeEndpoint{}, fmt.Errorf("daemon has no VS Code supervisor")
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
		return vscodeEndpoint{}, err
	}
	if instance.UserKilled() {
		return vscodeEndpoint{}, fmt.Errorf("session %q has been killed", title)
	}
	worktree := instance.GetWorktreePath()
	if strings.TrimSpace(worktree) == "" {
		return vscodeEndpoint{}, fmt.Errorf("session %q has no worktree to open in VS Code", title)
	}
	key := daemonInstanceKey(repoID, title)
	endpoint, err := m.vscode.ensureServer(key, worktree)
	// The post-spawn recheck below must run on errVSCodeStarting too, NOT just on
	// success. ensureServer REGISTERS the server in v.servers before returning that
	// sentinel, so a cold spawn that merely outran the start grace has left a LIVE,
	// daemon-owned code-server behind — the "error" reports a slow start, not a
	// failed one. Bare-returning here (as this did) skipped the recheck for exactly
	// the spawns that take longest, which is precisely when a concurrent
	// close/archive/kill is most likely to have won the race. The notice page's
	// auto-refresh cannot heal it either: the retry re-enters WebTabTarget, the tab
	// id no longer resolves, and it 404s without ever touching the supervisor — so
	// the editor would live until daemon shutdown.
	//
	// Every OTHER error means no server was registered, so there is nothing to
	// reclaim and the recheck would be pointless work.
	if err != nil && !errors.Is(err, errVSCodeStarting) {
		return vscodeEndpoint{}, err
	}
	if unwanted := m.stopVSCodeIfUnwanted(instance, key, title); unwanted != nil {
		return vscodeEndpoint{}, unwanted
	}
	if err != nil {
		// Still starting, and still wanted: let the caller render the retry notice.
		return vscodeEndpoint{}, err
	}
	return endpoint, nil
}

// stopVSCodeIfUnwanted re-checks, AFTER a spawn, that the editor still has a
// reason to exist — stopping it and returning why if not, or nil if it should be
// served. It is the closing half of the spawn window: this route deliberately
// does NOT take the op-lock (a spawn blocks for seconds and would stall
// KillSession/ArchiveSession/CloseTab behind it), so every condition
// ensureVSCodeServer checked BEFORE the spawn can change during it. Checking only
// before would leave the window wide open for exactly the seconds a spawn takes.
//
// All three questions must be re-asked, because they fail independently:
//
//   - TabSpawnBlocked: archive raises its fence (BeginArchive) and then MOVES the
//     worktree. An editor started against the old path now serves bytes that are
//     gone. ArchiveTeardown keeps the vscode tab (it is metadata-only, #1817), so
//     the tab check alone says "still wanted" and cannot catch this. That was
//     masked while archive still (wrongly) stripped the tab — the tab check
//     returned false and stopped the editor BY ACCIDENT — so fixing the archive
//     drop is what makes this check load-bearing rather than theoretical.
//   - UserKilled: kill does not filter tabs off the stale instance pointer this
//     request holds, so the tab check passes and the editor would survive against
//     a REMOVED worktree.
//   - instanceHasVSCodeTab: CloseTab stops the editor under that same op-lock, so
//     the tab this request resolved can be closed — and its stopFor already run —
//     mid-spawn. The editor would then belong to no tab: nothing renders it, and
//     no close/archive/kill path for a tab that no longer exists will ever stop
//     it.
//
// The deferred sweeps in KillSession/ArchiveSession are the belt to this brace;
// this keeps the "inert session ⇒ no editor" invariant from resting on which
// goroutine reaches v.mu first.
func (m *Manager) stopVSCodeIfUnwanted(instance *session.Instance, key, title string) error {
	err := func() error {
		if err := instance.TabSpawnBlocked(); err != nil {
			return err
		}
		if instance.UserKilled() {
			return fmt.Errorf("session %q has been killed", title)
		}
		if !instanceHasVSCodeTab(instance) {
			return fmt.Errorf("the VS Code tab of session %q was closed", title)
		}
		return nil
	}()
	if err != nil {
		m.vscode.stopFor(key)
	}
	return err
}

// webTabProxyHandler reverse-proxies /v1/webtab/{sessionId}/{tabId}/{rest...}
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
	// A network browser can authorize an iframe's first navigation only through a
	// query parameter. Treat that parameter as a ONE-HOP bootstrap transport, not
	// as part of the preview app's address: store the already-authenticated value
	// in an HttpOnly cookie, then redirect to the exact same browser path with every
	// decoded spelling of only our private parameter removed.
	//
	// This runs before manager access by design. Resolving a VS Code target may
	// START the editor, and arbitrary preview code must never run while its own
	// window.location still contains the daemon bearer. The clean cookie-backed
	// follow-up is the first request allowed to resolve or contact any target.
	if cleanWebTabTokenBootstrap(w, r) {
		return
	}
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, fmt.Errorf("daemon has no session manager"))
		return
	}
	// Refuse until the restore has finished (#1878). The HTTP listener binds long
	// before it (#829, deliberately), so a stale iframe left open across a daemon
	// restart starts re-requesting the moment the port answers — and every request
	// resolves through resolveStreamSession, which calls refreshLocked and REPLACES
	// the instance map from disk. The proxy was doing lifecycle work that
	// RestoreInstances documents as its own: "every RPC that mutates it is gated on
	// Ready". This route is HTTP rather than net/rpc, so it slipped that gate and a
	// pre-warm-up request drove its own restore.
	//
	// It answers a NOTICE rather than writeHTTPError's JSON envelope: the pane
	// frames this route, so an error body is rendered AT THE USER. A raw envelope in
	// the iframe is the exact failure the editor's notice pages exist to avoid, and
	// this reply is the likeliest of all to be seen — a daemon restart points every
	// open pane at it at once. Retry is set, so a pane caught mid-restore resolves
	// into its content on its own, with no reload.
	//
	// Kind-agnostic by necessity AND by rights: resolving the kind is the very thing
	// that touches the manager, so it is not known here — and a dev-server preview
	// must not be told VS Code is starting.
	if err := cs.requireStateMutationAdmission(); err != nil {
		if IsDaemonUpgradeProbationErr(err) {
			writeTabNoticePage(w, "Validating upgrade", "af is validating a daemon upgrade — this tab will reload when validation finishes.", true)
			return
		}
		writeTabNoticePage(w, "Starting up", "af is starting up — this tab will load as soon as the daemon has restored its sessions.", true)
		return
	}
	sessionID := r.PathValue("sessionId")
	tabID := r.PathValue("tabId")
	rest := r.PathValue("rest")
	// Defense in depth: ServeMux cleans a LITERAL ".." out of the path (an
	// unescaped /../ is redirected away before any handler sees it), but an ENCODED
	// one — %2E%2E%2F — is NOT cleaned: it decodes on the way into rest and arrives
	// here intact. Reject the residue so a crafted request can never escape the
	// proxied prefix. rest is the decoded view of the remainder, so testing it here
	// covers the escaped form the proxy actually forwards.
	//
	// Only a whole SEGMENT equal to ".." climbs. Testing for ".." anywhere in the
	// string also rejected legitimate routes that merely contain it — /assets/
	// bundle..js and friends never reached the dev server at all (#2104).
	if hasDotDotSegment(rest) {
		writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}
	// The remainder in the request's OWN encoding, which rest cannot express: the
	// forwarded path is built from this so an encoded slash survives the hop.
	escapedRest, ok := escapedRestOf(r.URL.EscapedPath())
	if !ok {
		writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}
	upstreamEscaped := "/" + strings.TrimLeft(escapedRest, "/")
	upstreamPath, err := url.PathUnescape(upstreamEscaped)
	if err != nil {
		// Unreachable via a real request (net/http rejects a malformed escape while
		// parsing the request line), but fail closed rather than forward a path
		// whose two encodings disagree.
		writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}

	// Addressed by the tab's STABLE id: a stale id (its tab was closed) is a clean
	// 404 here, never a silent bind to whatever tab took its old ordinal (#1810).
	target, err := cs.manager.WebTabTarget(sessionID, tabID)
	tabKind := target.Kind
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
		// An editor that started and then exited without ever listening is a broken
		// install/config, not a transient state: render it INTO the pane like the
		// other two, since the iframe shows this document and a raw JSON error
		// envelope is unreadable there.
		//
		// Deliberately NON-retrying, unlike the still-starting notice: the
		// supervisor records this failure and replays it for a cooldown rather than
		// respawning, so a self-refreshing page would spend that whole window
		// re-rendering the same replayed error — the UI fighting the very cooldown
		// that exists to stop a spawn loop.
		if errors.Is(err, errVSCodeStartExited) {
			writeVSCodeNoticePage(w, "VS Code exited while starting. Check that the editor binary runs correctly, then reopen this tab.")
			return
		}
		writeHTTPError(w, r, http.StatusNotFound, err)
		return
	}
	// Only loopback targets are proxied. An external target must never be fetched
	// by the daemon (open-proxy / SSRF) — the web UI iframes those directly.
	//
	// A unix-socket target is exempt because the check does not APPLY to it, not
	// because it is trusted less carefully. IsLoopbackWebTarget asks "does this
	// name a host off this machine?", and a socket names no host at all: it is a
	// path the daemon itself chose inside a directory only the daemon can write
	// (#1873). There is no address for an attacker to point anywhere, which is a
	// stronger guarantee than the string check the TCP path settles for — under
	// which the old editor target passed precisely because it WAS loopback, the
	// confused-deputy hole this transport closes.
	if !target.isUnixSocket() && !session.IsLoopbackWebTarget(target.URL) {
		writeHTTPError(w, r, http.StatusBadRequest,
			fmt.Errorf("web tab target %q is not loopback; external URLs are iframed directly, not proxied", target.URL))
		return
	}
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, fmt.Errorf("invalid web tab target: %w", err))
		return
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
		// A socket-bound editor is reached by DIALING THE PATH: the URL's host is
		// the dummy vscode.invalid and never resolves, so the transport must
		// replace the dial rather than the address. Everything above the dial —
		// the path mirror, the cookie and Location rewrites, the WS upgrade — is
		// unchanged, which is the point: only the transport moved (#1873).
		//
		// nil Transport (a web tab) keeps http.DefaultTransport, whose connection
		// pooling and proxy env handling this must not disturb.
		Transport: target.roundTripper(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = targetURL.Host
			// The browser path mirrors the upstream path, so the remainder under
			// the tab prefix IS the upstream path: forward it VERBATIM, in the
			// browser's own encoding. Path and RawPath are set TOGETHER — decoded
			// and escaped views of one string — so url.String() reproduces that
			// encoding instead of re-canonicalizing it, and a %2F that is data
			// inside a segment reaches the dev server as %2F rather than turning
			// into a path separator that names a different route.
			//
			// Deriving both from the escaped path also keeps the property the
			// decoded wildcard gave for free: a literal "?"/"#"/"%" in a filename
			// arrives already escaped (%3F/%23/%25) and stays that way, so it can
			// never be misread as a query/fragment/escape.
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = upstreamEscaped
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
			// Strip ONLY the daemon's own credential, and do it at the STRING level
			// so the target's query survives byte-for-byte. Parsing and re-encoding
			// (url.Values.Encode) would sort the params and rewrite escaping (a
			// literal space becomes %20→+), silently changing an order- or
			// signature-sensitive dev endpoint — the exact preservation targetQueryOf
			// promises on the client. The app's own params, including its own
			// ?access_token= (a DIFFERENT name from the daemon's), ride through
			// untouched.
			pr.Out.URL.RawQuery = stripRawQueryParam(pr.Out.URL.RawQuery, webtabTokenQueryParam)
			pr.SetXForwarded()
			// SetXForwarded derives X-Forwarded-Proto from the DAEMON-facing hop,
			// which OVERWRITES what the client's own hop reported (#1875). The
			// daemon's listener is plain HTTP by design, so behind a TLS-terminating
			// front proxy — the recommended network deployment — an inbound
			// "X-Forwarded-Proto: https" became "http" on the way upstream, and the
			// dev server was told an https:// page was plain HTTP. An app that builds
			// absolute URLs or a WS endpoint from that header then emits http://ws://
			// under an https:// page, which the browser blocks as mixed content.
			//
			// The ORIGINAL client's scheme is the honest answer, so it is restored
			// here — for BOTH tab kinds, since one Rewrite serves them and a plain dev
			// server reads this header exactly as an editor does.
			//
			// Resolved to a single value rather than forwarding the chain verbatim:
			// requestIsHTTPS already applies the first-entry rule (a chain may read
			// "https, http"), and plenty of upstreams test this header by exact match,
			// so handing them "https, http" would read as not-https and fix nothing.
			// The first entry IS the value every reader wants.
			//
			// Trusted only to UPGRADE, matching the reasoning on requestIsHTTPS: a
			// forged header buys a peer nothing but http:// links from an https:// page
			// for itself, and authenticates nothing — the auth gate still verifies the
			// token.
			if requestIsHTTPS(pr.In) {
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// The proxied preview is served same-origin as the SPA, so a dev server
			// that sends X-Frame-Options would block its own preview from framing.
			// Strip it (and the frame-ancestors CSP directive) so the loopback
			// preview always renders — this only affects the user's own dev server,
			// viewed through their own daemon.
			resp.Header.Del("X-Frame-Options")
			stripFrameAncestors(resp.Header)
			// The upstream ANSWERED, so whatever it says, this is not an af-generated
			// failure — strip any marker it set before the client can read it as one
			// (#1909). Without this an app could forge af's own dead-server verdict
			// against itself: its answered 502 would suppress its page and show the
			// fallback, the very bug the marker fixes, in reverse.
			//
			// This is the STRIP half of the #1879 rule — what the client trusts, the
			// proxy must control — and the two halves cannot collide: ModifyResponse
			// runs only when the upstream answered, the ErrorHandler only when it did
			// not. Del is canonical-key based, so a lowercase forgery is caught too.
			resp.Header.Del(webtabErrorHeader)
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
			// Mark this 502 as AF's OWN before writing it (#1909). Reaching here means
			// the upstream never answered — the transport failed, or ModifyResponse
			// rejected the response — so no upstream header has been copied to w and
			// this marker cannot be an upstream's. That is precisely what makes it
			// trustworthy: the client renders its dead-server fallback for a marked
			// 502 and the app's own page for an unmarked one.
			//
			// Set before writeHTTPError: that writes the status, after which headers
			// no longer reach the client.
			w.Header().Set(webtabErrorHeader, webtabErrorUpstreamUnreachable)
			writeHTTPError(w, r, http.StatusBadGateway,
				fmt.Errorf("web tab dev server at %s is unreachable: %w", targetURL.Host, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

// cleanWebTabTokenBootstrap persists a credential presented directly by the
// caller and, when the private query transport is present, redirects to the same
// request URI without it. It reports whether it wrote that redirect.
//
// The caller is behind withAuth, so a query value reaching this point has already
// been compared with the daemon's token. Existing cookie-authorized requests do
// not reissue the cookie: only a credential PRESENTED in the header or query does.
func cleanWebTabTokenBootstrap(w http.ResponseWriter, r *http.Request) bool {
	presented := agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader))
	if presented == "" {
		presented = r.URL.Query().Get(webtabTokenQueryParam)
	}
	if presented != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     webtabTokenCookie,
			Value:    presented,
			Path:     webtabPathPrefix,
			HttpOnly: true,
			Secure:   requestIsHTTPS(r),
			SameSite: http.SameSiteStrictMode,
		})
	}

	cleanRawQuery := stripRawQueryParam(r.URL.RawQuery, webtabTokenQueryParam)
	if cleanRawQuery == r.URL.RawQuery {
		return false
	}

	cleanURL := *r.URL
	cleanURL.RawQuery = cleanRawQuery
	cleanURL.ForceQuery = false
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, cleanURL.RequestURI(), http.StatusTemporaryRedirect)
	return true
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
