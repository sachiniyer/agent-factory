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
//  3. ORIGIN, for a browser that sends no fetch metadata (Safari before 16.4,
//     Firefox before 90, Chrome before 76). The Fetch standard attaches Origin to
//     every request whose method is not GET or HEAD, and to every WebSocket
//     handshake, so an Origin that does not name this daemon — scheme://Host as the
//     request was addressed — is refused, "null" included. When fetch metadata IS
//     present, a "same-origin" verdict is the browser's own attestation and Origin
//     is not compared again: a same-host proxy that rewrites Host (nginx's default
//     proxy_pass sends 127.0.0.1:8443) would otherwise refuse the operator's own web
//     UI for no gain.
//  4. CONTENT TYPE, on any request that can carry a body (not GET, HEAD or OPTIONS):
//     a declared Content-Type must be application/json, and a body must declare one.
//     This is the layer that does not depend on any browser being modern: an HTML
//     form can only send form-urlencoded, multipart or text/plain, and a JSON content
//     type from a page needs a CORS preflight, which this daemon grants only to the
//     origins in layer 1. A POST with no body and no Content-Type (curl -X POST, the
//     web client's two body-less calls) is admitted: the only browser requests that
//     can lack a Content-Type are fetch/XHR/beacon calls, and those always carry the
//     Origin layer 3 judges.
//
// WHERE. Every TCP listener's control plane: the daemon's web listener and the
// agent-server (whose Go client has sent application/json with every body since it
// shipped, and no Origin). Not the preview origin, whose whole path space belongs to
// the previewed app, and not /v1/webtab/ on the control listener for the same
// reason: a framed app's own requests come from a sandboxed (opaque-origin) frame —
// cross-site by definition — and go to the app's dev server or editor, never to an
// af RPC; that surface keeps its own posture (the sandbox, unguessable session and
// tab ids, code-server's WebSocket Origin check). Not the unix sockets: a browser
// cannot reach one, and the documented `curl --unix-socket … -d` examples keep
// working. Non-browser clients (the af CLI and TUI, the in-sandbox af, curl) send no
// fetch metadata and no Origin, so only layer 4 ever applies to them.

// crossSiteDenial is one refusal: the status and the sentence that explains it.
type crossSiteDenial struct {
	status int
	err    error
	// logKey groups refusals for the rate-limited log; never derived from the body.
	logKey string
}

// isControlPlanePath reports whether a request path is af's own API rather than a
// framed app's (/v1/webtab/…). The path is cleaned first so a dot-segment cannot
// wear the web-tab prefix into an RPC route.
func isControlPlanePath(p string) bool {
	clean := path.Clean("/" + p)
	if clean != "/v1" && !strings.HasPrefix(clean, "/v1/") {
		return false
	}
	return !strings.HasPrefix(clean+"/", webtabPathPrefix)
}

// crossSiteRefusal applies the layers above to r. cors is the posture's
// cors_allowed_origins (the same once-per-request read as the rest of the posture).
func crossSiteRefusal(r *http.Request, cors []string) *crossSiteDenial {
	if r.URL == nil || !isControlPlanePath(r.URL.Path) {
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" || !originAllowed(origin, cors) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			switch strings.ToLower(strings.TrimSpace(site)) {
			case "same-origin", "none":
			default:
				return &crossSiteDenial{
					status: http.StatusForbidden,
					err: fmt.Errorf("refused a %s browser request (Sec-Fetch-Site: %s): this API answers only "+
						"its own web client and non-browser clients. To call it from a web client you host "+
						"elsewhere, add that origin to network.cors_allowed_origins", echoHost(site), echoHost(site)),
					logKey: "site:" + strings.ToLower(echoHost(site)) + " " + echoHost(origin),
				}
			}
		} else if origin != "" && !originNamesRequest(origin, r) {
			return &crossSiteDenial{
				status: http.StatusForbidden,
				err: fmt.Errorf("refused a browser request from Origin %q, which is not this daemon's own origin. "+
					"To call the API from a web client you host elsewhere, add that origin to "+
					"network.cors_allowed_origins", echoHost(origin)),
				logKey: "origin:" + echoHost(origin),
			}
		}
	}
	return jsonBodyRefusal(r)
}

// originNamesRequest reports whether a browser Origin is this daemon's own origin as
// the request was addressed: the same host name as the Host header (canonicalized
// like the Host guard's), the same port — a Host with no port takes the default port
// of the Origin's scheme, which is how a browser omits it — and, when the daemon
// knows the request arrived over HTTPS (TLS, or X-Forwarded-Proto from a TLS proxy
// such as tailscale serve), the https scheme. A TLS proxy that sends no
// X-Forwarded-Proto is indistinguishable from plain HTTP, so without that signal
// either scheme is accepted; the port still has to match.
func originNamesRequest(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	oname, ok := config.RequestHostName(u.Host)
	if !ok {
		return false
	}
	hname, ok := config.RequestHostName(r.Host)
	if !ok || oname != hname {
		return false
	}
	if requestIsHTTPS(r) && u.Scheme != "https" {
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
	hport := defaultPort
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "" {
		hport = p
	}
	return oport == hport
}

// jsonBodyRefusal is layer 4: a request that can carry a body must declare JSON if
// it declares anything, and must declare it if it has a body.
func jsonBodyRefusal(r *http.Request) *crossSiteDenial {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		if r.ContentLength == 0 {
			return nil
		}
		return &crossSiteDenial{
			status: http.StatusUnsupportedMediaType,
			err: errors.New("this API needs Content-Type: application/json on a request body; " +
				"with curl, add -H 'Content-Type: application/json' (or use --json)"),
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
		log.WarningLog.Printf("refused %s %s from %s (Origin %q, Sec-Fetch-Site %q, Content-Type %q): %v%s",
			r.Method, r.URL.Path, r.RemoteAddr, echoHost(r.Header.Get("Origin")),
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
