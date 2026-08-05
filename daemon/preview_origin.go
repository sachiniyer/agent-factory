package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/session"
)

// The PER-TAB preview ORIGIN (#1856 step 3b). Each web tab is served from its own
// hostname on the preview listener —
//
//	http://af<32 base32 chars>.localhost:<preview_port>/
//
// — where the tab's dev server owns the origin ROOT. Two properties follow, and
// both are the point of the whole issue:
//
//  1. ABSOLUTE-PATH ASSETS RESOLVE. On the app origin a preview is mirrored under
//     /v1/webtab/<sid>/<tid>/, so an app that emits /assets/app.js escapes the
//     prefix and 404s (#1811 made that failure honest; it did not fix it). Here the
//     browser resolves /assets/app.js against the tab's OWN origin root, which is
//     exactly the upstream path — correct by construction, no Referer heuristic
//     (impossible under an opaque-origin frame, see the #1856 measurements) and no
//     ambiguity with two previews open.
//  2. CROSS-TAB READS ARE ISOLATED. Tab A and tab B are DIFFERENT origins, so A's
//     dev-server JS fetching B's URL is a cross-origin read, and the preview
//     listener emits no Access-Control-Allow-Origin (denyCORS, forced independent
//     of cors_allowed_origins), so the browser refuses to expose the response.
//     Isolation is a property of the ORIGIN, not of the credential: cookies are
//     scoped by (host, path) and NOT by port (RFC 6265 §8.5), so on the single
//     shared origin step 2/3a left, a sibling-path fetch still had B's cookie
//     ambiently attached and could read the answer. Only a distinct origin closes it.
//
// THE HOSTNAME IS THE CREDENTIAL. The label is previewTabHostLabel — an unguessable
// HMAC over (sid, tid) under the in-memory Manager.previewSecret — and the gate
// authenticates it (previewOriginAuth), so a request whose Host names no minted tab
// is 401, and one tab's origin never addresses another's.
//
// Moving the per-tab credential from step 3a's cookie/query transport INTO the host
// is not a downgrade, it is what makes the credential survive the origin split. The
// per-tab origin is CROSS-SITE with the SPA: "localhost" is an effective TLD, so
// http://localhost:8443 and http://af….localhost:8444 are different sites, which
// means the frame is a third-party context. There, 3a's SameSite=Strict cookie is
// never sent at all, SameSite=None needs Secure over a plain-HTTP listener, and a
// browser with third-party cookies blocked withholds it regardless. A credential
// that the browser may silently decline to send is not a credential — every
// sub-resource would 401 and the preview would render blank. The Host header is sent
// on every request by construction, so the capability rides the one channel the
// origin split cannot take away. Everything else from 3a survives unchanged: the
// ephemeral secret, the per-tab HMAC derivation, the request-aware gate, and
// /v1/preview-auth as the only way to obtain a tab's address.
//
// What the label leaks, honestly: an app that fetches an EXTERNAL URL sends its own
// origin in Referer/Origin, so a third party can learn the label. That is materially
// inert — the name resolves to the RECIPIENT's own loopback, never the operator's
// box, and a browser page that did reach the port still cannot read the response
// (no ACAO). A local process could, but a local process can already reach the
// control plane, which is tokenless on loopback by default. The dev server itself
// never sees the label: the proxy rewrites EVERY header that carries the
// browser-facing origin — Host, X-Forwarded-Host, Origin and Referer — to the
// target's own address (rewriteOriginHeader). That set is the invariant: any future
// header quoting the request's origin has to join it, or the credential ends up in
// an ordinary dev-server request log.
//
// REMOTE VIEWERS ARE UNCHANGED. A browser off this machine resolves *.localhost to
// ITS OWN loopback, so per-tab origins are localhost-only by nature. The web client
// therefore uses them only when the SPA itself is loaded from a loopback address and
// falls back to the same-origin, opaque-sandboxed /v1/webtab/ mirror otherwise —
// which is exactly today's behavior, unchanged, for every remote deployment.

// previewHostSuffix is the parent domain every per-tab preview origin sits under.
// RFC 6761 reserves it: Chromium and Firefox resolve any *.localhost name to
// loopback internally, without a DNS query, which is what makes a per-tab hostname
// free to mint and impossible to point off-box.
//
// It is required, not merely conventional. Accepting an arbitrary parent would let
// a per-tab origin be addressed through attacker-controlled DNS (af<label>.evil.tld
// resolving to 127.0.0.1), and while the label still authenticates, there is no
// reason to widen the surface for a deployment af does not support: a remote viewer
// cannot use per-tab origins at all (see the file header), so no real front needs it.
const previewHostSuffix = ".localhost"

// previewLabelPrefix starts every per-tab host label. It exists so a label is
// recognizable as af's BEFORE any secret is consulted: previewShellHandler answers
// a request whose Host carries no af-shaped label with a plain explanatory 404
// rather than routing it into the gate, so an operator who opens the preview port
// directly gets a sentence instead of a bare 401. It also keeps the label starting
// with a letter, which the oldest DNS label grammar (RFC 1035) requires.
const previewLabelPrefix = "af"

// previewLabelBytes is how much of the HMAC the label carries. 20 bytes is 160 bits
// of unguessability — far beyond what a local, rate-unlimited attacker could search
// — and encodes to exactly 32 base32 characters, keeping the whole label at 34, well
// inside the 63-character DNS label limit even with the prefix.
const previewLabelBytes = 20

// previewLabelEncoding renders the label bytes as base32 without padding: the
// alphabet is [A-Z2-7], which is DNS-safe, and lowercasing it keeps the host in the
// canonical form a browser sends (Host comparison is case-insensitive, but emitting
// the canonical spelling means the client, the daemon, and any log agree verbatim).
var previewLabelEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// previewOriginRegistryMax caps how many per-tab origins one daemon keeps
// addressable. A label is deterministic per (sid, tid), so re-registering a tab
// replaces its entry and real usage is bounded by tabs actually previewed; the cap
// exists only so a caller that already holds the daemon bearer cannot grow the map
// without bound by minting origins for ids that name nothing. Eviction is
// oldest-first and self-heals: the web client re-registers a tab every time it
// mounts its pane.
const previewOriginRegistryMax = 1024

// previewTabRef is the tab a per-tab origin addresses. Stored rather than derived
// because the label is a one-way HMAC: the daemon can compute a tab's label, never
// read a label back into a tab.
type previewTabRef struct {
	sessionID string
	tabID     string
}

// previewOriginRegistry maps a minted host label back to its tab. It is the ONLY
// way a preview-origin request resolves to a session — there is no path component to
// read a tab id out of, because the tab owns the whole path space.
//
// Registration happens in /v1/preview-auth, behind the daemon bearer, so an origin
// exists only after an already-authorized control-plane client asked for it. That
// also keeps the gate free of any manager access: the lookup is a map read under one
// mutex, so the auth path can never take a session lock, and it cannot be tempted
// into resolving a session before RestoreInstances has run (the #1878 trap).
type previewOriginRegistry struct {
	mu      sync.Mutex
	byLabel map[string]previewTabRef
	// order records insertion sequence for the cap's oldest-first eviction. A label
	// appears at most once: re-registering an existing label leaves its position.
	order []string
}

func newPreviewOriginRegistry() *previewOriginRegistry {
	return &previewOriginRegistry{byLabel: make(map[string]previewTabRef)}
}

// register makes label addressable for (sessionID, tabID), evicting the oldest
// entries once the cap is exceeded.
func (p *previewOriginRegistry) register(label, sessionID, tabID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.byLabel[label]; !exists {
		p.order = append(p.order, label)
	}
	p.byLabel[label] = previewTabRef{sessionID: sessionID, tabID: tabID}
	for len(p.order) > previewOriginRegistryMax {
		oldest := p.order[0]
		p.order = p.order[1:]
		delete(p.byLabel, oldest)
	}
}

// lookup resolves a host label to its tab. ok=false for a label this daemon never
// minted — including every label from a PREVIOUS daemon, since the secret is
// in-memory and rotates on restart.
func (p *previewOriginRegistry) lookup(label string) (previewTabRef, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref, ok := p.byLabel[label]
	return ref, ok
}

// previewTabHostLabel derives the DNS label of one tab's preview origin: the
// base32 of the first previewLabelBytes of HMAC-SHA256("host\x00<sid>\x00<tid>")
// under secret, lowercased and prefixed.
//
// The "host" domain separator keeps this derivation distinct from any other use of
// the same secret, so a future credential derived from previewSecret can never
// collide with a host label. The NUL separators make the (sid, tid) pair
// unambiguous — ids cannot contain a NUL byte — so no concatenation collision can
// let one tab's origin address another's.
func previewTabHostLabel(secret, sessionID, tabID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("host"))
	mac.Write([]byte{0})
	mac.Write([]byte(sessionID))
	mac.Write([]byte{0})
	mac.Write([]byte(tabID))
	return previewLabelPrefix + strings.ToLower(previewLabelEncoding.EncodeToString(mac.Sum(nil)[:previewLabelBytes]))
}

// previewHostLabel extracts the af label from a request's Host header, or ok=false
// when the host is not a per-tab preview origin at all (the bare bound address, a
// bare "localhost", a foreign name).
//
// It requires the EXACT shape af mints — one af-prefixed label of the right length
// and alphabet, directly under previewHostSuffix — so nothing else can be mistaken
// for a tab address. The port is dropped (cookies and origins differ by port, but a
// host LABEL does not), the name is lowercased (Host is case-insensitive), and a
// single trailing dot (the DNS root label) is stripped, mirroring
// session.IsLoopbackWebTarget's treatment of the same spelling.
func previewHostLabel(host string) (string, bool) {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	label, found := strings.CutSuffix(name, previewHostSuffix)
	if !found || !isPreviewLabel(label) {
		return "", false
	}
	return label, true
}

// previewHostIsProbe reports whether a request's Host names the reserved
// reachability-probe origin. It shares previewHostLabel's normalization (port
// dropped, lowercased, one trailing root dot stripped) so the two agree on what a
// host IS, and it is checked FIRST because the probe label is deliberately not a
// valid tab label.
func previewHostIsProbe(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	return name == previewProbeLabel+previewHostSuffix
}

// isPreviewLabel reports whether s has the exact shape previewTabHostLabel mints.
// Checked structurally rather than by regexp so the constants above are the single
// source of truth for the label's length and alphabet.
func isPreviewLabel(s string) bool {
	rest, found := strings.CutPrefix(s, previewLabelPrefix)
	if !found || len(rest) != previewLabelEncoding.EncodedLen(previewLabelBytes) {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// hasIframeTab reports whether sessionID names a live session holding an IFRAME
// tab with stable id tabID. It is the registration guard on /v1/preview-auth, and
// it is deliberately NOT WebTabTarget: resolving a vscode target SPAWNS an editor,
// which minting an address must never do.
//
// Resolved under ONE lock (TabTargetByID), the same discipline the proxy uses: an
// id→ordinal then ordinal→tab pair takes the instance lock twice, and a tab closing
// between the two leaves the index in range but pointing at a different tab.
//
// A not-yet-ready manager answers false rather than resolving anything. That is the
// #1878 rule — the HTTP listener binds long before RestoreInstances, and
// resolveStreamSession's refresh would otherwise load a session off disk here — and
// it fails the safe way: no origin is minted, so the client keeps the same-origin
// mirror until the daemon has restored.
func (m *Manager) hasIframeTab(sessionID, tabID string) bool {
	if !m.Ready() {
		return false
	}
	// The ALREADY-RESTORED map, never resolveStreamSession. That resolver's miss path
	// runs findSessionByStableID + refreshLocked, i.e. it reads session state off disk
	// — and this validation sits on a GET that, under the default tokenless posture,
	// any cross-origin page can drive without being able to read the reply. Resolving
	// through the refreshing path would let a page force repeated disk work simply by
	// embedding preview-auth URLs with random session ids: the guard added to stop the
	// registry being filled would have handed over a cheaper amplifier instead.
	//
	// A miss here means "not in the restored map", which is exactly the right answer
	// for minting: an origin is only ever vended for a session the daemon already
	// holds. Nothing legitimate depends on the refresh — the web client asks about a
	// tab it is currently rendering.
	instance := m.trackedStreamSession(sessionID)
	if instance == nil {
		return false
	}
	kind, _, ok := instance.TabTargetByID(tabID)
	if !ok {
		return false
	}
	return kind == session.TabKindWeb || kind == session.TabKindVSCode
}

// previewOriginFor returns the browser-facing origin of one tab's preview —
// "http://<label>.localhost:<port>" — and registers the label so the preview
// listener will resolve it. It returns "" when there is no origin to give: the
// preview listener is not BOUND (disabled, or its bind failed), or (sessionID,
// tabID) names no live iframe tab.
//
// The tab check is what keeps this GET from being a WRITE anyone can drive. The
// control listener is tokenless for every peer by default (require_token=false), so
// any page in the user's browser can issue a cross-origin no-cors GET here — it
// cannot read the answer, but the request still runs. Registering unvalidated pairs
// would let it mint arbitrary labels until the registry cap evicted the REAL ones,
// and every open preview would fail until its pane remounted. Bounded to live tabs,
// the map cannot be grown past what the user actually has open: a label is
// deterministic per (sid, tid), so re-registering the same tab replaces its entry.
//
// The port comes from the RESOLVED bound address, so a preview_listen_addr of
// "127.0.0.1:0" yields the real ephemeral port, and a wildcard bind ("0.0.0.0:8444")
// still yields a loopback-resolving *.localhost name rather than the wildcard host.
func previewOriginFor(m *Manager, sessionID, tabID string) string {
	if m == nil || m.lifecycle == nil {
		return ""
	}
	listeners := m.lifecycle.snapshot().listeners
	if !listeners.PreviewBound {
		return ""
	}
	_, port, err := net.SplitHostPort(listeners.PreviewBoundAddr)
	if err != nil || port == "" {
		return ""
	}
	if !m.hasIframeTab(sessionID, tabID) {
		return ""
	}
	label := previewTabHostLabel(m.previewSecret, sessionID, tabID)
	m.previewOrigins.register(label, sessionID, tabID)
	return "http://" + label + previewHostSuffix + ":" + port
}

// previewOriginAuth is the preview listener's credential wiring: the request's OWN
// host label is the presented credential, and the expected one is re-derived from
// the tab that label is registered for. A host that names no minted tab yields an
// empty expected token, which ConstantTimeEqual rejects — fail-closed.
//
// The re-derivation is deliberate rather than a bare map hit: it re-proves the
// binding between the label and the tab under the live secret, so a registry entry
// can only ever authorize the tab whose label actually hashes to it.
func previewOriginAuth(m *Manager) *tcpListenerAuth {
	return &tcpListenerAuth{
		expectedForRequest: func(r *http.Request) (string, error) {
			label, ok := previewHostLabel(r.Host)
			if !ok {
				return "", nil
			}
			ref, ok := m.previewOrigins.lookup(label)
			if !ok {
				return "", nil
			}
			return previewTabHostLabel(m.previewSecret, ref.sessionID, ref.tabID), nil
		},
		presented: func(r *http.Request) string {
			label, _ := previewHostLabel(r.Host)
			return label
		},
	}
}

// newPreviewMux is the handler mounted on the preview listener. There is exactly
// ONE route, and that is the design: the tab owns its origin's whole path space, so
// every path — including /v1/... — belongs to the previewed dev server. Reserving
// any af path here would shadow a real route of the app being previewed, and af has
// no need of one: the credential is the hostname and the control API lives on the
// other listener.
func newPreviewMux(cs *controlServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", cs.previewOriginHandler)
	return mux
}

// previewOriginHandler serves one request on a per-tab preview origin: it resolves
// the tab from the request's Host label and hands the SAME proxy core the app-origin
// route uses (serveWebTabRoute) a route rooted at "/" — the tab owns the origin, so
// its browser path prefix is empty and the request path IS the upstream path.
//
// Everything the mirrored route guarantees is therefore guaranteed here by
// construction rather than by a parallel implementation: the escaped path is
// forwarded verbatim, an encoded ".." is rejected, the tab is resolved under ONE
// lock by its stable id, the readiness gate holds, and framed failures render as
// notice pages rather than JSON envelopes.
func (cs *controlServer) previewOriginHandler(w http.ResponseWriter, r *http.Request) {
	if cs.manager == nil {
		writeHTTPError(w, r, http.StatusServiceUnavailable, errors.New("daemon has no session manager"))
		return
	}
	// #2400's clean-before-render rule, kept on this origin too. Nothing af mints
	// ever puts a credential in a preview-origin URL — the hostname carries it — so
	// this only ever fires for a hand-pasted or bookmarked leftover, and it fires
	// BEFORE any target is resolved so no preview code renders under a document
	// whose own location holds an af bearer.
	if stripAfCredentialQuery(w, r) {
		return
	}
	label, ok := previewHostLabel(r.Host)
	if !ok {
		// Unreachable through the gate (it denies a host with no af label), so this is
		// fail-closed defense rather than a live path.
		writeHTTPError(w, r, http.StatusNotFound, errors.New("not a web-tab preview origin"))
		return
	}
	ref, ok := cs.manager.previewOrigins.lookup(label)
	if !ok {
		// Not reachable through the gate, which denies an unregistered label first and
		// renders the SAME page (servePosture's previewOrigin branch). Kept as the
		// fail-closed floor for any future wiring that reaches this handler directly:
		// a label the registry does not hold must never resolve to a tab.
		writePreviewExpiredPage(w)
		return
	}
	// The remainder BELOW the tab's browser prefix, in the request's OWN encoding and
	// WITHOUT a leading slash — the same shape the app-origin route's {rest...}
	// wildcard yields. Trimming the slash is what makes the two routes' "rest" mean
	// the same thing, so a bare origin-root hit ("/" ⇒ "") takes the mirror redirect
	// exactly as a bare tab-root hit does.
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	rest, err := url.PathUnescape(escaped)
	if err != nil {
		// Unreachable via a real request (net/http rejects a malformed escape while
		// parsing the request line), but fail closed rather than forward a path whose
		// two encodings disagree.
		writeHTTPError(w, r, http.StatusBadRequest, errors.New("invalid web tab path"))
		return
	}
	cs.serveWebTabRoute(w, r, webTabRoute{
		sessionID: ref.sessionID,
		tabID:     ref.tabID,
		// Empty: the tab owns the origin ROOT, so there is no prefix to mirror under.
		// Every prefix-aware rewrite (cookie Path, Location, Refresh) degenerates to
		// the identity here, which is precisely what "the app owns its origin" means.
		prefix:        "",
		escapedRest:   escaped,
		rest:          rest,
		previewOrigin: true,
	})
}

// stripAfCredentialQuery redirects to the same URL with every af credential query
// param removed, and reports whether it wrote that redirect. It is
// cleanBootstrapToken's scrub half WITHOUT the cookie half: the preview origin has
// no query-borne credential to persist (its credential is the hostname), so a param
// found here is a leftover to be removed, never one to be stored.
func stripAfCredentialQuery(w http.ResponseWriter, r *http.Request) bool {
	clean := r.URL.RawQuery
	for _, p := range afCredentialQueryParams {
		clean = stripRawQueryParam(clean, p)
	}
	if clean == r.URL.RawQuery {
		return false
	}
	cleanURL := *r.URL
	cleanURL.RawQuery = clean
	cleanURL.ForceQuery = false
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, cleanURL.RequestURI(), http.StatusTemporaryRedirect)
	return true
}

// writePreviewExpiredPage renders the one thing a preview origin says when it
// cannot resolve a tab: the address is stale. It is a NOTICE page, not
// writeHTTPError's JSON envelope, because this route is FRAMED — whatever it
// returns is what the user sees where their app should be.
//
// One renderer, two callers (the gate's denial and the handler's floor), because
// they describe the same state: a well-formed host label that names no live tab.
// The everyday cause is a daemon restart, which rotates the in-memory secret and
// empties the registry, so every iframe left open keeps addressing a dead name.
//
// Deliberately NON-retrying, unlike the warm-up notice: this address can never
// become valid again under the new secret, so a self-refreshing page would spin
// forever. Reloading the web UI mints a fresh origin, which is the real recovery.
func writePreviewExpiredPage(w http.ResponseWriter) {
	writeTabNoticePage(w, "Preview address expired",
		"This preview address is no longer valid — the daemon restarted since it was opened. "+
			"Reload the af web UI to get a fresh one.", false)
}

// previewProbeLabel is a RESERVED host label the preview listener answers with a
// tiny page, unauthenticated, so the web client can ask the only question that
// actually decides whether per-tab origins are usable: can THIS BROWSER reach the
// preview port at all?
//
// It exists because no server-side signal can answer that. The daemon cannot tell
// a same-machine browser from one on the far end of `ssh -L 8443:127.0.0.1:8443`
// — both arrive on loopback, and both see a window.location of
// http://localhost:8443 — yet only the first can reach af<label>.localhost:<preview
// port>. Guessing wrong costs the user a working preview: the client would abandon
// the tunnelled mirror for an address that resolves to the viewer's own machine.
// The same probe answers the browser-support question (Safari does not resolve
// *.localhost at all) with no user-agent sniffing.
//
// It is a FRAMED probe rather than a fetch on purpose: the SPA's CSP is
// `default-src 'self'`, so every fetch/XHR/WebSocket to another origin is blocked,
// while `frame-src ... http:` is already permissive for web tabs. The page posts a
// fixed string to its parent and nothing else; the client checks the message came
// from the frame it created.
//
// It is deliberately NOT a valid tab label (too short for isPreviewLabel), so it
// can never collide with a minted origin, and it discloses strictly less than the
// explanatory 404 the same listener already serves unauthenticated: that a preview
// listener is here. No state, no session, no tab.
const previewProbeLabel = "afprobe"

// previewProbeMessage is what the probe page posts to its parent. A fixed,
// secret-free string: the SIGNAL is that it arrived at all.
const previewProbeMessage = "af-preview-origin-ok"

// writePreviewProbePage answers the reserved probe host. It is served before the
// gate (an unauthenticated client has no per-tab credential to present, and the
// answer carries nothing worth authenticating) and is no-store so a stale cached
// yes can never outlive a listener that has gone away.
func writePreviewProbePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The page must be able to run its one line under the client's sandbox, which
	// grants allow-scripts and nothing else — so it has an opaque origin and can do
	// nothing but post this message.
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>af preview probe</title></head>`+
		`<body><script>parent.postMessage(`+strconv.Quote(previewProbeMessage)+`,"*")</script></body></html>`)
}

// previewShellHandler is the preview listener's outermost wrapper. It answers a
// request whose Host carries no af-shaped label with a plain, address-free 404
// explaining what the port is, and passes everything else through to the authed
// handler untouched.
//
// It sits OUTSIDE the gate for the same reason webShellHandler's static branch does:
// a human who opened the preview port in a browser has no credential to present, so
// gating the explanation would leave them staring at a bare 401 about a bearer token
// that plays no part on this listener. The check is on label SHAPE only — never on
// whether a label is registered — so it discloses nothing about what is running: a
// well-formed but unknown label still takes the gate's 401.
func previewShellHandler(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if previewHostIsProbe(r.Host) {
			// The reachability probe, answered before the gate and before the label
			// check — see previewProbeLabel for why this cannot be a fetch.
			writePreviewProbePage(w)
			return
		}
		if _, ok := previewHostLabel(r.Host); !ok {
			writeHTTPError(w, r, http.StatusNotFound, errors.New(noPreviewContentMessage))
			return
		}
		api.ServeHTTP(w, r)
	})
}

// previewOriginBanner is the one-line operator notice logged when the preview
// listener binds, naming what it now serves. Its shape is the honest summary of the
// design: previews only, one hostname per tab, and localhost-only by nature.
func previewOriginBanner(addr string) string {
	return fmt.Sprintf("web-tab preview listener bound on %s (serves web-tab previews only, "+
		"each tab on its own http://<id>%s origin so absolute-path assets resolve; "+
		"per-tab origins are same-machine only — a remote viewer keeps the sandboxed same-origin preview)",
		addr, previewHostSuffix)
}
