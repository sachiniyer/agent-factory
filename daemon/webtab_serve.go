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

// The web-tab reverse proxy's SERVE core, shared by the two ways a tab is
// addressed (#1856 step 3b):
//
//	app origin      http://<daemon>/v1/webtab/<sid>/<tid>/<upstream path>
//	preview origin  http://af<label>.localhost:<preview>/<upstream path>
//
// They differ in exactly one value — the browser path PREFIX the tab hangs under,
// "/v1/webtab/<sid>/<tid>" on the app origin and "" on its own preview origin — and
// in nothing else. That is why they share one implementation rather than two:
// every invariant this route keeps being re-broken on (#1806, #1810, #1811, #1858,
// #1865, #1875, #1878) is a property of this function, so a second copy would be a
// second place to lose them. The prefix-aware rewrites (Set-Cookie Path, Location,
// Refresh, X-Forwarded-Prefix) all degenerate to the identity at prefix "", which is
// precisely what "the tab owns its origin root" means.

// webTabRoute is one resolved addressing of a web tab: which tab, the browser path
// prefix its URLs hang under, and the upstream remainder in both encodings.
type webTabRoute struct {
	sessionID string
	tabID     string
	// prefix is the browser-visible path every URL of this tab sits under, with NO
	// trailing slash: "/v1/webtab/<sid>/<tid>" on the app origin, "" on a per-tab
	// preview origin. It is what upstream Set-Cookie paths, Location headers and
	// Refresh URLs are re-scoped under, and what an editor is told through
	// X-Forwarded-Prefix.
	prefix string
	// escapedRest is the remainder BELOW prefix in the request's own
	// percent-encoding, without a leading slash — the one thing r.PathValue("rest")
	// cannot give (see escapedRestOf). The forwarded upstream path is built from
	// this, so a %2F that is data inside a segment survives the hop instead of
	// turning into a separator that names a different upstream route.
	escapedRest string
	// rest is escapedRest decoded — the same string the mux's {rest...} wildcard
	// yields. The ".." check runs on this because an ENCODED %2E%2E%2F is what
	// ServeMux does not clean, and it decodes into exactly this view.
	rest string
	// previewOrigin marks a request served on the tab's OWN origin (#1856 step 3b),
	// where the app owns the whole path space and no other origin may read it. It is
	// an explicit flag rather than a prefix=="" test because three behaviors turn on
	// it and none of them is really about the prefix being empty:
	//
	//   - no mirror-root redirect: "/" is the APP's root here, not a place to bounce
	//     back to the target's path;
	//   - upstream CORS headers are stripped, so a dev server cannot hand another
	//     tab's origin permission to read it;
	//   - X-Forwarded-Host is overwritten with the target's, so the unguessable host
	//     label never reaches the dev server.
	previewOrigin bool
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
// The one thing the mirror still cannot express is an ABSOLUTE-path asset
// (/assets/app.js): it resolves against the daemon origin's root and escapes the
// prefix, so it 404s honestly (#1811) rather than loading. That is what the per-tab
// PREVIEW origin fixes (previewOriginHandler), by giving the tab an origin whose
// root IS the upstream root — this route stays exactly as it is, and remains the
// only one a REMOTE viewer can use.
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
	sessionID := r.PathValue("sessionId")
	tabID := r.PathValue("tabId")
	// The remainder in the request's OWN encoding, which r.PathValue("rest") cannot
	// express: the forwarded path is built from this so an encoded slash survives.
	escapedRest, ok := escapedRestOf(r.URL.EscapedPath())
	if !ok {
		writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}
	cs.serveWebTabRoute(w, r, webTabRoute{
		sessionID:   sessionID,
		tabID:       tabID,
		prefix:      webtabPathPrefix + sessionID + "/" + tabID,
		escapedRest: escapedRest,
		rest:        r.PathValue("rest"),
	})
}

// requestOwnOrigin is the origin THIS request was addressed to, as the browser would
// spell it in an Origin header: the scheme it actually reached af on, plus its Host.
// It is what rewriteOriginHeader compares against, so only a header naming this very
// tab's origin is rewritten and a foreign one is left for the upstream to judge.
//
// It deliberately does NOT use requestIsHTTPS, and the difference is the point.
// That helper honors X-Forwarded-Proto because for its purpose — telling the upstream
// what scheme the user's page is on — a forged value buys a peer nothing but broken
// links for itself. Here the value decides IDENTITY, and a client-settable input
// deciding identity is a bypass in both directions: send `X-Forwarded-Proto: https`
// and the browser's real `Origin: http://…` stops matching, so the tab's own header
// is misread as foreign and forwarded verbatim — leaking the capability label this
// rewrite exists to contain; send the matching https spelling too and a foreign
// origin gets laundered instead.
//
// So the scheme comes from the connection alone. The preview origin is plain HTTP by
// construction: af terminates no TLS, and per-tab origins are *.localhost, which is
// same-machine only — there is no legitimate TLS-terminating front on this route for
// r.TLS to be missing behind.
func requestOwnOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// serveWebTabRoute is the proxy itself, for a tab already resolved to a route. It
// is reached from BOTH origins (webTabProxyHandler, previewOriginHandler) and holds
// every invariant of the route in one place.
func (cs *controlServer) serveWebTabRoute(w http.ResponseWriter, r *http.Request, route webTabRoute) {
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
	if hasDotDotSegment(route.rest) {
		writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("invalid web tab path"))
		return
	}
	upstreamEscaped := "/" + strings.TrimLeft(route.escapedRest, "/")
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
	target, err := cs.manager.WebTabTarget(route.sessionID, route.tabID)
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
			fmt.Errorf("web tab target %q is not loopback; external URLs are iframed directly, not proxied",
				agentproto.RedactAccessTokenURL(target.URL)))
		return
	}
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		writeHTTPError(w, r, http.StatusInternalServerError, fmt.Errorf("invalid web tab target: %w",
			agentproto.RedactAccessTokenError(err, "")))
		return
	}

	// The path prefix this tab's cookies are scoped under. Upstream Set-Cookie
	// paths are rewritten beneath it so a cookie-backed dev app (login/session/
	// CSRF) works in the iframe without its cookies colliding with the daemon's
	// own /v1/webtab/ token cookie or leaking to a sibling tab. Because the browser
	// path mirrors the upstream path, this is a pure prefix-prepend and the
	// re-scoped cookie lands on exactly the requests the app scoped it to.
	//
	// On a per-tab PREVIEW origin it is empty, and the app's cookies keep their own
	// paths verbatim — they cannot collide with a sibling tab because the HOST
	// differs, and rewriteSetCookiePaths drops Domain either way, so a dev server
	// that tried to set Domain=localhost (which WOULD be shared by every per-tab
	// origin) is re-scoped to a host-only cookie for its own tab.
	tabPathPrefix := route.prefix

	// Keep the browser-visible URL mirroring the upstream one: a bare hit on the
	// tab root is sent to the target's own path, after which every relative URL the
	// app emits resolves at the right depth on its own.
	//
	// MIRRORED ROUTE ONLY. On a per-tab preview origin "/" is the app's REAL root —
	// its home link, its fetch("/") — and bouncing that to the target's subpath would
	// mean the app could never address its own root on an origin it supposedly owns.
	// The mirror exists to make a prefixed URL resolve at the upstream's depth; with
	// no prefix the path already IS the upstream path, so there is nothing to mirror.
	// The client points the initial frame straight at the target's path, so nothing
	// depends on this redirect there.
	if route.rest == "" && !route.previewOrigin {
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
			// The upstream Host is the TARGET's, never the browser's. On a per-tab
			// preview origin that is also what keeps the tab's unguessable host label
			// off the wire: the dev server is told the address it serves itself on.
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
			// Set only for a vscode tab, and only when there IS a prefix: on a per-tab
			// preview origin the editor already owns the root, and announcing an empty
			// prefix would only invite an upstream to build "" -relative URLs. For a
			// web tab the target is an arbitrary dev server, and a framework that
			// honored this header would start rewriting its URLs — a behavior change to
			// today's previews that belongs in its own change, not smuggled in here.
			if tabKind == session.TabKindVSCode && tabPathPrefix != "" {
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
			// Strip BOTH af credential query params before forwarding: af_webtab_token
			// (this gate's) and af_preview_token (the preview origin's retired query
			// transport, which a bookmark from an older release can still carry). They
			// share a cookie jar and a path scope (RFC 6265 §8.5 — cookies are not
			// port-scoped), so a crafted request to this origin can carry either, and
			// the strip set must cover everything ANY af gate has ever accepted, never
			// just this one's (the webtab_url.go rule; the daemon-bearer half of it is
			// why the first strip exists). Neither may reach the untrusted dev server.
			pr.Out.URL.RawQuery = stripRawQueryParam(pr.Out.URL.RawQuery, webtabTokenQueryParam)
			pr.Out.URL.RawQuery = stripRawQueryParam(pr.Out.URL.RawQuery, previewTokenQueryParam)
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
			// SetXForwarded copies the INBOUND Host into X-Forwarded-Host — and on a
			// per-tab preview origin that host is the unguessable label that IS the
			// tab's credential. Forwarding it would hand the capability to the very
			// dev server the origin exists to contain, where any request log keeps it.
			// pr.Out.Host is already the target's; this makes the header agree with it,
			// which is also the honest answer (it is the host the app serves itself on).
			// Skipped for a SOCKET-backed target, whose "host" is the dummy
			// vscode.invalid: rewriting to a name that never reaches a wire would tell
			// code-server its browser-facing host does not exist, and it validates its
			// WebSocket upgrade against X-Forwarded-Host — the editor would load and
			// then fail to connect. The exemption is principled: these rewrites keep a
			// tab's capability label away from UNTRUSTED repo/agent-controlled content,
			// and a socket target is a code-server the daemon itself spawned, which
			// would learn only a label authorizing its own tab. Found on #2743's review;
			// applied here too, since this PR extends the same rewrite to two more
			// headers and would otherwise reintroduce it through them.
			if route.previewOrigin && !target.isUnixSocket() {
				pr.Out.Header.Set("X-Forwarded-Host", targetURL.Host)
				// X-Forwarded-Host was one INSTANCE of a class, not the whole of it.
				// ReverseProxy clones the inbound headers, so every header that carries
				// the BROWSER-facing origin reaches the dev server verbatim — and on this
				// route that origin is the tab's unguessable capability label. Origin (on
				// any non-GET fetch the app makes) and Referer (on essentially every
				// sub-resource) both carry it, so a dev server that logs requests — Vite,
				// Django, Rails, all of them by default — writes the credential into its
				// log file.
				//
				// Both are rewritten to the target's own address rather than deleted:
				// that is what the app would have seen served directly, so an
				// Origin-checking CSRF guard now agrees with its own Host instead of
				// seeing a name it does not recognize. Referer keeps its path and query,
				// since only the origin part is the secret.
				rewriteOriginHeader(pr.Out.Header, targetURL, requestOwnOrigin(pr.In))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// A dev server that sends X-Frame-Options would block its own preview from
			// framing — on the app origin because the frame is same-origin as the SPA,
			// and on a per-tab preview origin because the frame is CROSS-origin to it,
			// which SAMEORIGIN refuses just as flatly. Strip it (and the
			// frame-ancestors CSP directive) so the loopback preview always renders —
			// this only affects the user's own dev server, viewed through their own
			// daemon.
			resp.Header.Del("X-Frame-Options")
			stripFrameAncestors(resp.Header)
			// Cross-tab read isolation is the whole point of the per-tab origin, and it
			// holds only while NO Access-Control-Allow-Origin comes back. Forcing the
			// daemon's own allow-list empty (livePosture.previewOrigin) is not enough:
			// this proxy relays the dev server's response headers, and a dev server
			// setting `Access-Control-Allow-Origin: *` is not exotic — Vite's dev server
			// does it by default. Relayed, that header lets tab A's JS read tab B's
			// response, which is exactly the escalation the separate origin prevents.
			//
			// So the upstream's CORS grants are stripped on this route. It costs the app
			// nothing real: its own same-origin fetches never consult CORS, and a grant
			// it makes to a THIRD party is that other server's header, not this one's.
			// The mirrored route is deliberately untouched — it is same-origin with the
			// SPA and has always relayed these.
			if route.previewOrigin {
				stripCORSGrants(resp.Header)
			}
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
			// this tab's proxy path (and Domain dropped so it defaults to the proxy
			// host) so the cookie lands on the right path and coexists with the
			// daemon's token cookie.
			rewriteSetCookiePaths(resp.Header, tabPathPrefix, route.previewOrigin)
			// Send the app's own redirects back through the prefix rather than out
			// to the daemon's origin, which is where a bare "/login" would otherwise
			// land (#1843).
			rewriteRedirectLocation(resp, tabPathPrefix, targetURL)
			rewriteRefreshURL(resp.Header, tabPathPrefix, targetURL)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, proxyReq *http.Request, err error) {
			// ReverseProxy reports the request context ending as a transport
			// error. That means the browser navigated away, disconnected, or the
			// tab was torn down; there is no live client to warn or manufacture a
			// dead-server response for. Keep genuine live-request transport
			// failures on the warning + marked-502 path below.
			if proxyReq != nil && errors.Is(err, proxyReq.Context().Err()) {
				return
			}
			safeErr := agentproto.RedactAccessTokenError(err,
				agentproto.AccessTokenFromQuery(targetURL.Query()))
			log.WarningLog.Printf("web tab proxy to %s failed: %v",
				agentproto.RedactAccessTokenURL(targetURL.String()), safeErr)
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
			writeHTTPError(w, proxyReq, http.StatusBadGateway,
				fmt.Errorf("web tab dev server at %s is unreachable: %w", targetURL.Host, safeErr))
		},
	}
	proxy.ServeHTTP(w, r)
}
