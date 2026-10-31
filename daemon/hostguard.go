package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The Host guard: the daemon's browser-facing listeners answer a request only
// when its Host names this daemon.
//
// WHY. The control listener is loopback-only and tokenless by default
// (require_token=false). The browser's same-origin policy keys on the NAME in the
// URL, not on the address it resolved to, so a page at http://attacker.example:8443
// whose name is re-pointed at 127.0.0.1 after load (DNS rebinding) is same-origin
// with whatever answers 127.0.0.1:8443. Before this guard the daemon answered such
// a request exactly like a same-machine one — the page could read every session and
// call every mutating route. The token gate cannot help: under the default there is
// no token, and isLoopbackRequest is (correctly) about the transport peer, which
// for a rebound page IS loopback. The Host header is the one part of the request
// the page cannot choose: a browser sets it from the URL, and fetch/XHR may not
// override it (a forbidden header name).
//
// WHAT NAMES THIS DAEMON (admitHost). In order:
//
//   - bare localhost and loopback IP literals, on any port
//     (config.IsLoopbackHostName). NOT a *.localhost subdomain: Safari before
//     macOS 26 resolves those through the system resolver and so the network's
//     DNS (WebKit bug 160504), where a hostile resolver can point one at an
//     attacker and rebind it. Admitting only af's own label SHAPE would not help
//     here — the shape is public, and these listeners do not hold the per-label
//     credential the preview listener checks — and nothing needs it: af's
//     *.localhost origins are all served by the preview listener.
//   - the host of the address this listener was bound from (network.listen_addr's
//     host), any port — for a wildcard bind that is the unspecified address a
//     browser sends when the operator types http://0.0.0.0:8443, an IP literal
//     no DNS can rebind.
//   - an IP literal equal to the address the client's connection actually reached
//     (http.LocalAddrContextKey). That is what covers a wildcard bind reached by its
//     LAN or tailnet IP. It cannot be steered by a page: an IP-literal URL has no
//     name to rebind, so a browser only sends Host: <our IP> when it connected to
//     our IP.
//   - the operator's network.allowed_hosts entries, any port — for a same-host
//     reverse proxy that forwards the browser's Host unchanged (tailscale serve does:
//     r.Out.Host = r.In.Host for a TCP backend).
//   - an EMPTY Host, only from a loopback transport peer. A browser never sends one
//     — HTTP/1.1 requires Host, Go's server already 400s an HTTP/1.1 request with
//     none, a page cannot override it, and a proxy forwarding a browser request
//     carries the browser's — so the only senders are local HTTP/1.0 tools.
//
// Everything else — a foreign name, a malformed Host (an unbracketed IPv6 literal,
// a non-numeric port, a zone, characters no hostname has), or an empty Host from a
// network peer — is refused with 421 before any routing: the static web UI, every
// /v1 route, and every WebSocket upgrade alike, because the guard is the
// listener's OUTERMOST handler.
//
// WHY 421. RFC 9110 §15.5.20: Misdirected Request — "the request was directed at a
// server that is unable or unwilling to produce an authoritative response for the
// target URI". That is exactly the condition: the target authority is not one this
// server answers for. 403 would say the requester lacks permission for a resource
// this server does serve, which misdescribes it, and would be indistinguishable in
// logs from an authorization refusal by the gate behind it.
//
// WHERE IT APPLIES. The control-plane web listener (startTCPListenerWithListen
// with withWebShell) and every per-session editor-origin port (editor_port_origin.go)
// — the two surfaces that serve a browser anything worth reaching. The web-tab
// preview listener has its own, stricter Host rule that predates this one:
// previewShellHandler, its outermost wrapper, answers only af-shaped
// af<label>.localhost names and the probe name and gives every other Host a fixed,
// address-free 404 before any gate or route; behind it the gate admits a label only
// if this daemon minted it (the label IS the credential), so a shape-valid label an
// attacker invented gets the expired notice, never a tab. No allowed_hosts entry
// could make a foreign name useful there. The agent-server listener is not a browser
// surface and its bearer token is mandatory for every peer, so a rebound page
// reaches nothing but 401s; a Host rule there would only break remotes addressed
// by a name.

// maxRefusedHostEcho bounds how much of a refused Host is echoed in the body and
// the log. Go accepts up to a megabyte of headers, and the Host is the requester's
// to choose.
const maxRefusedHostEcho = 200

// hostRefusal is why a request's Host was refused. suggest is the canonical name a
// network.allowed_hosts entry would need — "" when no entry could allow it (a
// malformed or missing Host).
type hostRefusal struct {
	host    string
	suggest string
	missing bool
}

// admitHost decides whether r is addressed to this daemon. listenHost is the
// canonical host of the address the listener was bound from
// (config.ListenAddrHostName; "" when unparseable); allowed is network.allowed_hosts. It
// reads nothing but its arguments and the request, so a caller decides where the
// allow-list comes from — for the control listener, the same once-per-request
// config read as the rest of its posture.
func admitHost(r *http.Request, listenHost string, allowed []string) (hostRefusal, bool) {
	if r.Host == "" {
		if isLoopbackRequest(r) {
			return hostRefusal{}, true
		}
		return hostRefusal{missing: true}, false
	}
	canon, ok := config.RequestHostName(r.Host)
	if !ok {
		return hostRefusal{host: r.Host}, false
	}
	if config.IsLoopbackHostName(canon) || (listenHost != "" && canon == listenHost) {
		return hostRefusal{}, true
	}
	// A wildcard listener's own spelling, in either family: Host: 0.0.0.0 on a
	// [::] dual-stack bind still names the bind the browser typed.
	if config.IsUnspecifiedHostName(canon) && config.IsUnspecifiedHostName(listenHost) {
		return hostRefusal{}, true
	}
	if hostIsConnLocalAddr(r, canon) {
		return hostRefusal{}, true
	}
	for _, entry := range allowed {
		// Entries are canonical after load; re-canonicalizing keeps a hand-built
		// Config (or a future writer that skips the loader) from silently missing.
		if c, err := config.CanonicalAllowedHost(entry); err == nil && c == canon {
			return hostRefusal{}, true
		}
	}
	return hostRefusal{host: r.Host, suggest: canon}, false
}

// hostIsConnLocalAddr reports whether canon is an IP literal equal to the local
// address of the connection r arrived on — the address the client dialled.
func hostIsConnLocalAddr(r *http.Request, canon string) bool {
	want, err := netip.ParseAddr(canon)
	if err != nil {
		return false
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false
	}
	host, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return false
	}
	got, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return got.WithZone("").Unmap() == want.Unmap()
}

// withHostGuard refuses every request whose Host does not name this daemon, then
// serves next. allowed is read once per request, so a network.allowed_hosts change
// applies on the next request. Used by listeners whose posture is not otherwise
// config-driven (the editor-origin ports, a static control listener in tests).
func withHostGuard(next http.Handler, listener, listenHost string, allowed func() []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refusal, ok := admitHost(r, listenHost, allowed()); !ok {
			refuseHost(w, r, listener, refusal)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLiveHostGuard is withHostGuard for the control listener, whose whole auth +
// CORS posture is read from live config exactly once per request (requestPosture).
// The allow-list must come from that SAME read: a request that took its Host
// verdict from one config generation and its token posture from the next could be
// admitted by a pair neither generation allows (an allowed_hosts entry from the old
// config, require_token=false from the new). So this builds the posture, judges
// the Host against it, and hands the very same posture to withLivePosture further
// in through the request context rather than letting it read again.
func withLiveHostGuard(next http.Handler, listener, listenHost string, build func() requestPosture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := build()
		if refusal, ok := admitHost(r, listenHost, p.allowedHosts); !ok {
			refuseHost(w, r, listener, refusal)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestPostureKey{}, &p)))
	})
}

// requestPostureKey carries the posture withLiveHostGuard already built for this
// request to withLivePosture. Only withLiveHostGuard writes it, and only with the
// listener's own build, so withLivePosture can never be handed a posture it would
// not have built itself.
type requestPostureKey struct{}

func requestPostureFrom(ctx context.Context) (requestPosture, bool) {
	p, ok := ctx.Value(requestPostureKey{}).(*requestPosture)
	if !ok || p == nil {
		return requestPosture{}, false
	}
	return *p, true
}

// refuseHost writes the 421 and logs the refusal (rate-limited). The body is
// plain text and readable by whoever sent the request — under DNS rebinding that
// is the attacking page — so it says only what the request itself already said
// plus the config line: never the existing allow-list, the config path, or any
// other name of this machine.
func refuseHost(w http.ResponseWriter, r *http.Request, listener string, refusal hostRefusal) {
	hostRefusals.record(listener, refusal, r.RemoteAddr)
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = fmt.Fprint(w, hostRefusalBody(refusal))
}

// hostRefusalBody is the refusal's text: the Host refused and, when a name could
// be allowed at all, the exact line that allows it.
func hostRefusalBody(refusal hostRefusal) string {
	switch {
	case refusal.missing:
		return "af refused this request: it carries no Host header. Requests from another machine must name the host they are addressed to.\n"
	case refusal.suggest == "":
		return fmt.Sprintf("af refused this request: Host %q is not a valid host name, so it cannot name this daemon.\n",
			echoHost(refusal.host))
	}
	// The command comes first: a remote `af` CLI that meets this refusal quotes only
	// the first 200 bytes of a non-envelope body (apiclient bodySnippet), and the
	// line that fixes it is the part worth quoting.
	return fmt.Sprintf("af refused Host %q: not a name of this daemon. To allow it, run on the af machine:\n"+
		"  af config set %s %s\n"+
		"or add it to the [network] table of af's config.toml:\n"+
		"  allowed_hosts = [%q]\n"+
		"Both replace the whole list, so keep any names already in it. A name needs allowing when you reach af "+
		"through it on purpose: behind a reverse proxy such as tailscale serve, or by a DNS name of this machine.\n",
		echoHost(refusal.host), config.AllowedHostsKey, refusal.suggest, refusal.suggest)
}

func echoHost(h string) string {
	if len(h) > maxRefusedHostEcho {
		return h[:maxRefusedHostEcho] + "…"
	}
	return h
}

// Rate limits for the refusal log. One line per (listener, Host) per interval,
// and at most hostRefusalLogBurst lines a minute across all of them: the Host is
// the requester's to vary, so a per-name limit alone would let a flood of distinct
// names flood the log. Suppressed refusals are counted into the next line.
const (
	hostRefusalLogInterval = 10 * time.Minute
	hostRefusalLogBurst    = 10
	hostRefusalLogMaxNames = 512
	// hostRefusalRecentMax bounds the refused names kept for `af doctor`.
	hostRefusalRecentMax = 8
)

// RefusedHost is one Host a browser-facing listener refused, as reported to `af
// doctor` over the control socket: the canonical name an allowed_hosts entry would
// need, the listener that refused it, how many refusals it has had since the
// daemon started (capped by what the log kept), and when the last one landed.
type RefusedHost struct {
	Name     string    `json:"name"`
	Listener string    `json:"listener"`
	Count    int       `json:"count"`
	Last     time.Time `json:"last"`
}

// hostRefusalLog is the daemon's record of refused Hosts: the rate-limited log
// and the short list `af doctor` reads.
type hostRefusalLog struct {
	mu          sync.Mutex
	now         func() time.Time
	logf        func(format string, args ...any)
	lastLogged  map[string]time.Time
	windowStart time.Time
	windowLines int
	suppressed  int
	recent      map[string]*RefusedHost
}

func newHostRefusalLog() *hostRefusalLog {
	return &hostRefusalLog{
		now:        time.Now,
		logf:       func(format string, args ...any) { log.WarningLog.Printf(format, args...) },
		lastLogged: map[string]time.Time{},
		recent:     map[string]*RefusedHost{},
	}
}

// hostRefusals is the daemon-wide record. Package-level because every refusing
// listener — the control listener's generations and each editor-origin port —
// shares one log budget, and doctor reads one list.
var hostRefusals = newHostRefusalLog()

func (l *hostRefusalLog) record(listener string, refusal hostRefusal, peer string) {
	l.mu.Lock()
	now := l.now()
	if refusal.suggest != "" {
		l.noteRecentLocked(listener, refusal.suggest, now)
	}
	key := listener + "\x00" + refusal.suggest
	if refusal.suggest == "" {
		key = listener + "\x00(unnamed)"
	}
	if t, ok := l.lastLogged[key]; ok && now.Sub(t) < hostRefusalLogInterval {
		l.mu.Unlock()
		return
	}
	if now.Sub(l.windowStart) >= time.Minute {
		l.windowStart, l.windowLines = now, 0
	}
	if l.windowLines >= hostRefusalLogBurst {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	if len(l.lastLogged) >= hostRefusalLogMaxNames {
		for k, t := range l.lastLogged {
			if now.Sub(t) >= hostRefusalLogInterval {
				delete(l.lastLogged, k)
			}
		}
		if len(l.lastLogged) >= hostRefusalLogMaxNames {
			l.suppressed++
			l.mu.Unlock()
			return
		}
	}
	l.lastLogged[key] = now
	l.windowLines++
	suppressed := l.suppressed
	l.suppressed = 0
	l.mu.Unlock()

	more := ""
	if suppressed > 0 {
		more = fmt.Sprintf(" (%d further refusals not logged)", suppressed)
	}
	switch {
	case refusal.missing:
		l.logf("%s refused a request from %s with no Host header%s", listener, peer, more)
	case refusal.suggest == "":
		l.logf("%s refused a request from %s with malformed Host %q%s", listener, peer, echoHost(refusal.host), more)
	default:
		l.logf("%s refused a request from %s addressed to Host %q, which is not a name of this daemon; "+
			"if that name is yours (a reverse proxy such as tailscale serve), allow it: af config set %s %s%s",
			listener, peer, echoHost(refusal.host), config.AllowedHostsKey, refusal.suggest, more)
	}
}

// noteRecentLocked keeps the doctor list: the most recently refused names, at most
// hostRefusalRecentMax, the oldest evicted first. Caller holds l.mu.
func (l *hostRefusalLog) noteRecentLocked(listener, name string, now time.Time) {
	key := listener + "\x00" + name
	if e, ok := l.recent[key]; ok {
		e.Count++
		e.Last = now
		return
	}
	if len(l.recent) >= hostRefusalRecentMax {
		var oldest string
		for k, e := range l.recent {
			if oldest == "" || e.Last.Before(l.recent[oldest].Last) {
				oldest = k
			}
		}
		delete(l.recent, oldest)
	}
	l.recent[key] = &RefusedHost{Name: name, Listener: listener, Count: 1, Last: now}
}

// snapshot returns the refused names, most recent first.
func (l *hostRefusalLog) snapshot() []RefusedHost {
	l.mu.Lock()
	out := make([]RefusedHost, 0, len(l.recent))
	for _, e := range l.recent {
		out = append(out, *e)
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// hostGuardListenerLabel names a control listener in the refusal log.
func hostGuardListenerLabel(addr string) string {
	return "web listener " + addr
}

// editorPortListenerLabel names an editor-origin port in the refusal log.
func editorPortListenerLabel(port int) string {
	return fmt.Sprintf("editor origin port %d", port)
}
