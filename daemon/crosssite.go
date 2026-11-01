package daemon

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The cross-site request guard: a browser request from another site never reaches
// an RPC handler or a WebSocket upgrade on a TCP listener's control plane, whatever
// the auth posture.
//
// WHY. Under the default posture (require_token=false, loopback bind) the control
// API needs no credential at all, and a browser will send some requests to any
// address with no CORS preflight: a POST whose Content-Type is form-urlencoded,
// multipart or text/plain, or that has no body. The page that sends one cannot read
// the answer, but the side effect happens — create a session with a prompt, send a
// prompt to an agent, kill or archive a session — which is code execution through
// the agents. A WebSocket handshake is not covered by CORS at all, and every daemon
// stream accepts with InsecureSkipVerify, so a page could also read and write a
// session's PTY. The Host guard (hostguard.go) does not help: the Host is a genuine
// 127.0.0.1.
//
// THE LAYERS. Judged in order, before routing, by crossSiteRefusal:
//
//  1. An Origin the operator listed in network.cors_allowed_origins is admitted: the
//     operator opted that site into cross-origin access, and it reaches the API the
//     way it did before (its preflight still gets the allow-list's headers).
//  2. FETCH METADATA, when the browser sent it. Sec-Fetch-Site is set by the browser
//     itself (a forbidden header a page cannot write), so it is the most reliable
//     signal there is: "same-origin" and "none" (the user typed or bookmarked the
//     URL) are admitted, and everything else is refused — "cross-site" and also
//     "same-site". Same-site is refused deliberately: on this machine it means
//     another PORT of the same host, which is where agents run untrusted dev servers
//     (http://localhost:3000 is same-site with http://localhost:8443), and nothing
//     af serves needs it. af's own *.localhost preview and editor origins never call
//     the control API at all — they are served by the preview listener and talk only
//     to their own origin — so whether a browser classifies them as same-site or
//     cross-site with localhost makes no difference here.
//  3. ORIGIN, whenever there is no fetch metadata. That is an older browser (Safari
//     before 16.4, Firefox before 90, Chrome before 76), but also EVERY Chromium
//     WebSocket handshake (Chromium sends no Sec-Fetch-* on one) and every request
//     to a plain-http, non-loopback address (browsers send fetch metadata only to
//     potentially trustworthy origins). The Fetch standard attaches Origin to every
//     request whose method is not GET or HEAD, and to every WebSocket handshake, so
//     an Origin that does not name this daemon is refused, "null" included. "Names
//     this daemon" means the same host and port as the Host header, or — from a
//     loopback peer, a same-host proxy that rewrote Host — as X-Forwarded-Host when
//     that is a name the daemon answers to (originNamesRequest), with
//     X-Forwarded-Port filling in a port the proxy dropped. When fetch metadata IS
//     present, a "same-origin" verdict is the browser's own attestation and Origin
//     is not compared again: a proxy that rewrites Host would otherwise refuse the
//     operator's own web UI for no gain.
//  4. CONTENT TYPE, on any request that can carry a body (not GET, HEAD or OPTIONS):
//     on the control listener it must declare application/json, even with no body.
//     This is the layer that does not depend on any browser being modern: an HTML
//     form can only send form-urlencoded, multipart or text/plain, a JSON content
//     type from a page needs a CORS preflight this daemon grants only to the origins
//     in layer 1, and a bare untyped POST is the one shape a browser too old to send
//     Origin on it (Firefox before 70) can send with no other signal. The web
//     client's two calls that carry nothing send {} as JSON. The agent-server alone
//     still admits a body-less untyped call, which is what the daemon driving it
//     sends (jsonBodyRefusal).
//
// WHERE. Every TCP listener's control plane: the daemon's web listener and the
// agent-server (whose Go client has sent application/json with every body since it
// shipped, and no Origin). Not the preview origin, whose whole path space belongs to
// the previewed app, and not /v1/webtab/ on the control listener for the same
// reason: a framed app's own requests come from a sandboxed (opaque-origin) frame —
// cross-site by definition — and go to the app's dev server or editor, never to an
// af RPC; that surface keeps its own posture (webtab_isolation.go: the sandbox, a
// relayed editor never on this origin, an editor's WebSocket only from its own
// origin by originNamesRequest below; and unguessable session and tab ids). Not the
// unix sockets: a browser cannot reach one, and the documented `curl --unix-socket
// … -d` examples keep working. Non-browser clients (the af CLI and TUI, the
// in-sandbox af, curl) send no fetch metadata and no Origin, so only layer 4 ever
// applies to them.

// crossSiteDenial is one refusal: the status and the sentence that explains it.
type crossSiteDenial struct {
	status int
	err    error
	// logKey groups refusals for the rate-limited log; never derived from the body.
	logKey string
}

// isFramedAppPath reports whether the daemon's router would hand r to the web-tab
// proxy (/v1/webtab/{sessionId}/{tabId}/{rest...}) — the one /v1 subtree that is a
// framed app's rather than af's. It decides that the way net/http.ServeMux does, not
// on a re-derived path: the mux cleans the ESCAPED path (and answers a path that
// cleaning changes with a redirect, never a handler) and then matches it segment by
// segment, unescaping each segment. Judging the decoded path instead lets the two
// disagree — "/v1/sessions/..%2F..%2Fx/stream" decodes to a path outside /v1 yet
// routes to the stream handler, and "/v1/..%2Fwebtab/…" decodes into the web-tab
// subtree yet routes elsewhere. Everything else that reaches servePosture on a TCP
// listener is control plane: the listener's shell only routes the decoded "/v1/"
// prefix there (webShellHandler, noWebShellHandler).
func isFramedAppPath(r *http.Request) bool {
	// A path that cleaning changes is answered with a redirect to the cleaned one and
	// runs no handler, so judge it by where it leads: a framed app's sloppy "//asset"
	// keeps its redirect, and the redirected request is judged again on arrival.
	escaped := muxCleanPath(r.URL.EscapedPath())
	segs := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(segs) < 5 {
		return false
	}
	for i, want := range []string{"v1", "webtab"} {
		seg, err := url.PathUnescape(segs[i])
		if err != nil || seg != want {
			return false
		}
	}
	sid, err1 := url.PathUnescape(segs[2])
	tid, err2 := url.PathUnescape(segs[3])
	return err1 == nil && err2 == nil && sid != "" && tid != ""
}

// muxCleanPath is net/http's cleanPath: the canonical form ServeMux redirects to.
func muxCleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

// crossSiteRefusal applies the layers above to r. cors and allowedHosts are the
// posture's cors_allowed_origins and allowed_hosts (the same once-per-request read
// as the rest of the posture); strictBodies is the control listener's rule that
// every request able to carry a body declares JSON (see jsonBodyRefusal).
func crossSiteRefusal(r *http.Request, cors, allowedHosts []string, strictBodies bool) *crossSiteDenial {
	if r.URL == nil || isFramedAppPath(r) {
		return nil
	}
	origin := r.Header.Get("Origin")
	// "null" is never an operator's origin: it is what a sandboxed frame, a file://
	// page or a redirect chain sends, so admitting a hand-edited "null" entry would
	// admit every one of them, WebSockets included.
	if origin == "" || origin == "null" || !originAllowed(origin, cors) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			switch strings.ToLower(strings.TrimSpace(site)) {
			case "same-origin", "none":
			default:
				return &crossSiteDenial{
					status: http.StatusForbidden,
					err: fmt.Errorf("refused a browser request with Sec-Fetch-Site %q: this API answers only "+
						"its own web client and non-browser clients. To call it from a web client you host "+
						"elsewhere, add that origin to network.cors_allowed_origins", echoHost(site)),
					logKey: "site:" + strings.ToLower(echoHost(site)) + " " + echoHost(origin),
				}
			}
		} else if origin != "" && !originNamesRequest(origin, r, allowedHosts) {
			return &crossSiteDenial{
				status: http.StatusForbidden,
				err: fmt.Errorf("refused a browser request from Origin %q, which is not this daemon's own origin. "+
					"Behind a reverse proxy that rewrites Host, forward the browser's with X-Forwarded-Host "+
					"(and X-Forwarded-Proto) or keep Host; to call the API from a web client you host "+
					"elsewhere, add that origin to network.cors_allowed_origins", echoHost(origin)),
				logKey: "origin:" + echoHost(origin),
			}
		}
	}
	return jsonBodyRefusal(r, strictBodies)
}

// originNamesRequest reports whether a browser Origin is this daemon's own origin as
// the browser addressed it. Two names can stand for that address:
//
//   - the Host header, as the request arrived;
//   - X-Forwarded-Host, but only from a LOOPBACK transport peer — a same-host
//     reverse proxy that rewrote Host (nginx with proxy_pass to 127.0.0.1) — and only
//     when that name is one the daemon answers to anyway: localhost, a loopback IP,
//     or a network.allowed_hosts entry. A page cannot set this header on a WebSocket
//     handshake or a no-cors request, and on a CORS request it is a custom header
//     that needs a preflight this daemon refuses. The admission requirement keeps a
//     rebinding name that a default-server proxy happens to route here from
//     vouching for itself.
//
// Against either, the Origin must have the same canonical host name (the Host
// guard's canonicalization) and the same port — a name with no port takes the
// default port of the Origin's scheme, which is how a browser omits it: tailscale
// serve forwards "Host: box.ts.net" for "Origin: https://box.ts.net", both port 443
// — and, when the daemon knows the request arrived over HTTPS (TLS, or
// X-Forwarded-Proto: https, which tailscale serve sends), the https scheme. A TLS
// proxy that sends no X-Forwarded-Proto is indistinguishable from plain HTTP, so
// without that signal either scheme is accepted; the port still has to match.
func originNamesRequest(origin string, r *http.Request, allowedHosts []string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	oname, ok := config.RequestHostName(u.Host)
	if !ok {
		return false
	}
	if requestIsHTTPS(r) && u.Scheme != "https" {
		return false
	}
	loopback := isLoopbackRequest(r)
	// X-Forwarded-Port restores a port a same-host proxy dropped (nginx's $host has
	// none). Like X-Forwarded-Host it counts only from a loopback peer.
	fport := ""
	if loopback {
		fport = firstForwardedValue(r.Header.Get("X-Forwarded-Port"))
		if fport != "" && !isDecimalPort(fport) {
			fport = ""
		}
	}
	if authority, ok := withForwardedPort(r.Host, fport, u); ok && originMatchesAuthority(u, oname, authority) {
		return true
	}
	if !loopback {
		return false
	}
	fwd := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	fname, ok := config.RequestHostName(fwd)
	if !ok || !forwardedNameAdmitted(fname, allowedHosts) {
		return false
	}
	authority, ok := withForwardedPort(fwd, fport, u)
	return ok && originMatchesAuthority(u, oname, authority)
}

// withForwardedPort fills in the port of a port-less authority from
// X-Forwarded-Port, and FAILS CLOSED when there is neither: a port-less authority
// whose Origin names an explicit port cannot vouch for it. A browser omits only
// the default port from Origin, so an explicit one is a non-default front door —
// and a proxy that forwarded "$host" (no port) for it has lost exactly the fact the
// comparison needs. Inferring the default port there would refuse the operator's
// own page and admit a page on the same host's default port instead. An authority
// that carries its own port (Host as the browser sent it, nginx's $http_host) is
// used as is; it wins over X-Forwarded-Port.
func withForwardedPort(authority, fport string, origin *url.URL) (string, bool) {
	if _, p, err := net.SplitHostPort(authority); err == nil && p != "" {
		return authority, true
	}
	if fport != "" {
		name, ok := config.RequestHostName(authority)
		if !ok {
			return "", false
		}
		return net.JoinHostPort(name, fport), true
	}
	if origin.Port() != "" {
		return "", false
	}
	return authority, true
}

// firstForwardedValue is the first entry of a comma-separated forwarding header —
// the one the outermost proxy set from the browser's own request.
func firstForwardedValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

func isDecimalPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

// originMatchesAuthority compares a parsed Origin (u, canonical name oname) with
// one host[:port] authority.
func originMatchesAuthority(u *url.URL, oname, authority string) bool {
	aname, ok := config.RequestHostName(authority)
	if !ok || aname != oname {
		return false
	}
	defaultPort := "80"
	if u.Scheme == "https" {
		defaultPort = "443"
	}
	oport := u.Port()
	if oport == "" {
		oport = defaultPort
	}
	aport := defaultPort
	if _, p, err := net.SplitHostPort(authority); err == nil && p != "" {
		aport = p
	}
	return oport == aport
}

// forwardedNameAdmitted reports whether a forwarded host name is one this daemon
// answers to without the listener's own address: a loopback name, or an operator's
// network.allowed_hosts entry.
func forwardedNameAdmitted(name string, allowedHosts []string) bool {
	if config.IsLoopbackHostName(name) {
		return true
	}
	for _, entry := range allowedHosts {
		if c, err := config.CanonicalAllowedHost(entry); err == nil && c == name {
			return true
		}
	}
	return false
}

// jsonBodyRefusal is layer 4: a request that can carry a body must declare JSON if
// it declares anything. On the control listener (strict) it must declare JSON even
// with no body: a bodyless POST with no Content-Type is the one browser request
// that carries neither a form type nor, in a browser too old to send Origin on it
// (Firefox before 70), any other signal — af's own clients all declare JSON, the web
// client included. The agent-server (not strict) admits a bodyless request with no
// Content-Type, because the daemon that drives it — every version since #1659 —
// sends exactly that for a body-less call, and its token is mandatory anyway.
func jsonBodyRefusal(r *http.Request, strict bool) *crossSiteDenial {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		if !strict && r.ContentLength == 0 {
			return nil
		}
		return &crossSiteDenial{
			status: http.StatusUnsupportedMediaType,
			err: errors.New("this API needs Content-Type: application/json on every POST, PUT, PATCH and DELETE, " +
				"even with no body; with curl, add -H 'Content-Type: application/json' (or use --json '{}')"),
			logKey: "content-type:(none)",
		}
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == "application/json" {
		return nil
	}
	return &crossSiteDenial{
		status: http.StatusUnsupportedMediaType,
		err: fmt.Errorf("this API accepts only Content-Type: application/json, got %q; "+
			"with curl, add -H 'Content-Type: application/json' (or use --json) instead of sending a form", echoHost(ct)),
		logKey: "content-type:" + echoHost(ct),
	}
}

// refuseCrossSite writes the refusal as the daemon's ordinary error envelope (a CLI
// meeting a 415 reads its message; a cross-site page can read nothing) and logs it,
// rate-limited, naming the route and the headers that decided it — never the body
// or the query.
func refuseCrossSite(w http.ResponseWriter, r *http.Request, d *crossSiteDenial) {
	if ok, suppressed := crossSiteLog.allow(d.logKey); ok {
		more := ""
		if suppressed > 0 {
			more = fmt.Sprintf(" (%d further refusals not logged)", suppressed)
		}
		// Every requester-chosen value is %q-quoted, the path included: a decoded path
		// can hold a newline (%0A), and an unquoted one forges log lines.
		log.WarningLog.Printf("refused %s %q from %s (Origin %q, Sec-Fetch-Site %q, Content-Type %q): %v%s",
			r.Method, echoHost(r.URL.Path), r.RemoteAddr, echoHost(r.Header.Get("Origin")),
			echoHost(r.Header.Get("Sec-Fetch-Site")), echoHost(r.Header.Get("Content-Type")), d.err, more)
	}
	writeHTTPError(w, r, d.status, d.err)
}

// keyedLogLimiter allows one log line per key per interval and at most burst lines
// a minute across all keys, counting what it suppressed so the next line can say so.
// The keys are partly requester-chosen (an Origin), so the overall burst is what
// bounds the log.
type keyedLogLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	interval    time.Duration
	burst       int
	maxKeys     int
	last        map[string]time.Time
	windowStart time.Time
	windowLines int
	suppressed  int
}

func newKeyedLogLimiter(interval time.Duration, burst, maxKeys int) *keyedLogLimiter {
	return &keyedLogLimiter{now: time.Now, interval: interval, burst: burst, maxKeys: maxKeys, last: map[string]time.Time{}}
}

func (l *keyedLogLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.interval {
		return false, 0
	}
	if now.Sub(l.windowStart) >= time.Minute {
		l.windowStart, l.windowLines = now, 0
	}
	if l.windowLines >= l.burst {
		l.suppressed++
		return false, 0
	}
	if len(l.last) >= l.maxKeys {
		for k, t := range l.last {
			if now.Sub(t) >= l.interval {
				delete(l.last, k)
			}
		}
		if len(l.last) >= l.maxKeys {
			l.suppressed++
			return false, 0
		}
	}
	l.last[key] = now
	l.windowLines++
	s := l.suppressed
	l.suppressed = 0
	return true, s
}

// crossSiteLog is the daemon-wide limiter for cross-site refusals.
var crossSiteLog = newKeyedLogLimiter(10*time.Minute, 10, 512)
