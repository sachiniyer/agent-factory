package daemon

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The plain-HTTP TCP listener for the daemon's HTTP/WS surface (#1592 Phase 3
// PR3, §1.1; HTTP-only rework 2026-07-14). It serves the SAME mux the unix
// socket serves, but wrapped in a token-enforcing gate: the unix socket is
// trusted transport (0600 perms are the auth, #1029) and passes a nil gate,
// while this listener requires a valid bearer token on every request (with the
// loopback exemption, #1696) and applies the CORS allow-list.
//
// It serves PLAIN HTTP — there is no TLS. af terminates no TLS of its own: a
// user who needs transport encryption terminates it at a reverse proxy
// (nginx/caddy) or runs over a private network (Tailscale/VPN/SSH tunnel). The
// bearer token authenticates the surface and now travels over the plaintext
// connection, so it MUST NOT be exposed on an untrusted network without one of
// those wrappers.
//
// It is ON BY DEFAULT — config.ListenAddr defaults to loopback
// ("127.0.0.1:8443"), so the bundled web UI is served on localhost out of the
// box. Only an explicit `listen_addr = ""` opts out: startHTTPServer then never
// calls in here and behavior is byte-identical to the pure-unix daemon that
// shipped before Phase 3.

// tcpListenerInfo is the enable-banner payload startHTTPServer logs once when
// the TCP listener binds. The token is included deliberately (§1.3): the daemon
// log is the operator's channel to the freshly generated credential — a
// documented log-file-readability consideration, gated behind the explicit
// listen_addr opt-in.
type tcpListenerInfo struct {
	Addr           string // the resolved bound address (host:port, port filled in for :0)
	Token          string // the bearer token clients must present
	done           <-chan struct{}
	closeRequested func() bool
}

// tokenGatePolicy is how a TCP listener's bearer-token gate treats peers. Its
// zero value is the strict, fail-safe posture — token mandatory for every peer,
// no exemptions — so a caller opts INTO relaxations explicitly (#1696):
//
//   - the daemon's own listen_addr web listener derives its policy from config
//     (webListenerPolicy). By DEFAULT that is {tokenDisabled: true} — require_token
//     defaults to false, so the daemon-served web UI needs no token from anyone;
//     require_token=true falls back to {loopbackExempt: true}, where a same-machine
//     browser needs no token while network peers do;
//   - the agent-server passes the zero value, keeping its token mandatory for
//     every peer (it exists to be reached over the network — the token must
//     never be optional there). It is NOT governed by require_token.
type tokenGatePolicy struct {
	// tokenDisabled drops the token for ALL peers (require_token=false, the
	// default). It short-circuits authRequired, so it overrides loopbackExempt.
	tokenDisabled bool
	// loopbackExempt lets 127.0.0.1/::1 peers skip the token.
	loopbackExempt bool
}

// webListenerPolicy is the token-gate posture for the daemon's own listen_addr
// web listener, derived from config. It relaxes the strict zero value in exactly
// two ways, and the loopback relaxation is bind-aware:
//
//   - tokenDisabled from require_token=false, THE DEFAULT — drop the token for ALL
//     peers, so the daemon-served web UI opens with no login. Paired with the
//     loopback-only default listen_addr, nothing off-host can reach it. A tokenless
//     gate CAN front a network listener: #2090 refused to bind that combination,
//     and #2168 Phase 0 reversed the refusal by owner decision, so startHTTPServer
//     binds it and warns once instead. Nothing below authenticates such a peer —
//     that is the configuration doing exactly what it says.
//   - loopbackExempt lets same-machine peers skip the token, BUT only when the
//     listener is LOOPBACK-BOUND. On a network bind the exemption is withheld
//     regardless of require_loopback_token: a same-host reverse proxy connects
//     from 127.0.0.1, so exempting loopback there would let anything behind the
//     proxy reach the control plane with no token. require_loopback_token=true
//     withdraws the exemption even on a loopback bind (shared/multi-user host).
//
// Because tokenDisabled short-circuits the gate, loopbackExempt only matters once
// require_token=true — require_loopback_token alone is inert under the default.
//
// The agent-server does NOT use this — it passes the strict zero-value policy
// (token mandatory for every peer) directly.
func webListenerPolicy(cfg *config.Config) tokenGatePolicy {
	return tokenGatePolicy{
		tokenDisabled:  !cfg.RequireToken,
		loopbackExempt: config.IsLoopbackListenAddr(cfg.ListenAddr) && !cfg.RequireLoopbackToken,
	}
}

// previewListenerPolicy is the token-gate posture for the web-tab preview
// listener (#1856). It is a SEPARATE seam from webListenerPolicy on purpose: the
// preview origin is not the control-plane listener and does not inherit its
// require_token/require_loopback_token posture.
//
// It returns the STRICT zero value — credential mandatory for every peer, no
// exemptions — deliberately. The preview origin serves untrusted, repo/agent-
// controlled content, and its credential is the PER-TAB origin label
// (previewOriginAuth), not the daemon bearer — so it must NOT inherit the control
// plane's require_token/require_loopback_token relaxations, under which the web UI
// opens with no token. Here the credential is mandatory for every peer, always: the
// gate re-derives the label of the tab the request's Host names and compares. A
// loopback browser is exactly the peer this must authenticate — exempting it would
// make every tab's preview readable from every other tab's origin.
func previewListenerPolicy(_ *config.Config) tokenGatePolicy {
	return tokenGatePolicy{}
}

// webShell selects whether a TCP listener also serves the embedded browser SPA.
// Only the daemon's own web listener does: the SPA speaks the daemon's REST
// surface (/v1/Snapshot, /v1/CreateSession, …), which the agent-server does not
// implement, so serving the shell there would hand a visitor a working-looking
// login screen that dead-ends on `unknown route "/v1/Snapshot"` the moment they
// pasted a token. Making this an explicit argument keeps "who serves the
// frontend" a decision at the call site rather than an accident of sharing
// startTCPListener.
type webShell int

const (
	// withoutWebShell answers every non-/v1 path with the agent-server's "no web UI
	// here, the daemon serves it at http://localhost:8443" 404. It is FIRST so it is
	// the zero value: like tokenGatePolicy and authGate's bool fields, an unset
	// webShell must default to the fail-safe posture (serve NO frontend), never to
	// mounting the SPA + its token-paste login on a listener that was never meant to
	// carry it (#1696 fail-safe-zero-value convention).
	withoutWebShell webShell = iota
	// withWebShell serves the browser SPA on every non-/v1 path — the daemon.
	withWebShell
	// previewShell wraps the preview listener in previewShellHandler, which answers a
	// request whose Host carries no af-shaped per-tab label with the preview origin's
	// OWN explanatory 404 and passes everything else to the gate. It must NOT reuse
	// withoutWebShell's message: that text names the control-plane address, which the
	// preview port must not advertise (least of all to an unauthenticated peer), and
	// "agent-server" is simply wrong here (#1856).
	//
	// Note it filters by HOST, not by path: on a per-tab preview origin the tab owns
	// the whole path space (including /v1/...), so a path filter would shadow a real
	// route of the app being previewed.
	previewShell
)

// tcpListenerAuth overrides the credential a listener's gate uses. Nil selects
// the default: the file-backed daemon bearer token (EnsureToken/LoadToken), used
// by the control-plane and agent-server listeners. The web-tab PREVIEW listener
// (#1856) passes a non-nil value to authenticate its SEPARATE per-tab credential on
// a distinct query/cookie transport, so the daemon bearer never reaches the preview
// origin.
type tcpListenerAuth struct {
	// expectedForRequest returns the credential THIS request must present, derived
	// per request (the preview listener's per-tab HMAC label, keyed on the tab its
	// Host names). It becomes authGate.expectedTokenForRequest. There is no single
	// credential to advertise, so the banner token is empty and per-tab origins are
	// vended via /v1/preview-auth. Fail-closed: an empty return denies (ConstantTimeEqual).
	expectedForRequest func(*http.Request) (string, error)
	// presented extracts the credential a request carries; it becomes
	// authGate.presentedToken (nil there ⇒ the webTabAwareToken default).
	presented func(*http.Request) string
}

// livePosture makes a TCP listener read its auth + CORS posture from LIVE config
// once per request (#2480 PR2) rather than baking the posture passed at bind time.
// Only the daemon's own web + preview listeners set it; the agent-server passes
// nil and keeps its fixed posture. It is how require_token / require_loopback_token
// / cors_allowed_origins take effect with no socket rebind — deliberately decoupled
// from the listen_addr / preview_listen_addr rebind path, so a security tightening
// applies on the next request whether or not any rebind succeeded.
type livePosture struct {
	// snapshot returns the current global config. It is read EXACTLY ONCE per
	// request (withLivePosture) and the whole posture derives from that one read.
	snapshot func() *config.Config
	// policyFromConfig derives the gate's token/loopback posture from the snapshot
	// (require_token / require_loopback_token) — the control-plane web listener. The
	// preview listener leaves it false: its gate posture is the fixed strict zero
	// value (previewListenerPolicy).
	policyFromConfig bool
	// previewOrigin marks the web-tab preview listener, which carries no control
	// plane and whose whole path space belongs to the previewed app. It is ONE flag
	// because it is one fact, and three consequences follow from it (all in
	// requestPosture.previewOrigin / servePosture):
	//
	//   - the cross-origin allow-list is forced EMPTY, ignoring cors_allowed_origins.
	//     This is the load-bearing half of #1856's cross-tab isolation: per-tab
	//     origins isolate a READ only while the browser refuses to expose one tab
	//     origin's response to another, which it does only while no
	//     Access-Control-Allow-Origin comes back. An operator sets
	//     cors_allowed_origins to let a separately-hosted UI call the CONTROL API;
	//     letting it reach the preview origin would hand tab A's dev-server JS a
	//     readable response from tab B.
	//   - the OPTIONS and /v1/auth-info shortcuts are withheld, so they cannot shadow
	//     a previewed app's own route or method.
	//   - a gate denial renders the framed notice instead of a JSON envelope.
	//
	// Zero value false keeps the control listener's behavior, and it is inert for the
	// agent-server (static posture, no livePosture at all).
	previewOrigin bool
}

// startTCPListener binds the plain-HTTP TCP listener on addr and serves mux
// wrapped in a token-enforcing gate + the CORS allow-list. It returns a cleanup
// function that shuts the server down and the banner payload the caller logs.
// policy selects how the gate treats peers (loopback exemption / token disable);
// its zero value is the strict "token mandatory for everyone" posture. shell
// selects whether the browser SPA rides along.
//
// addr is passed explicitly rather than read from cfg because the daemon binds
// TWO listeners from one cfg: the control-plane web listener on cfg.ListenAddr
// and the web-tab preview listener on cfg.PreviewListenAddr (#1856). cfg still
// supplies the shared token file and CORS allow-list; only the bind target
// differs.
//
// auth overrides the credential the gate enforces (see tcpListenerAuth); nil is
// the file-backed daemon bearer token. It ensures the bearer token exists before
// opening the port (so an operator enabling the listener always has a credential
// to present) and reads that token FRESH per auth event through the gate so `af
// token rotate` takes effect for new connections without a daemon restart. An
// override supplies its own already-minted credential and is compared as-is.
func startTCPListener(mux http.Handler, addr string, cfg *config.Config, policy tokenGatePolicy, shell webShell, auth *tcpListenerAuth, live *livePosture) (func() error, tcpListenerInfo, error) {
	return startTCPListenerWithListen(mux, addr, cfg, policy, shell, auth, live, net.Listen)
}

// startTCPListenerWithListen is startTCPListener with the socket constructor
// supplied by the restartable-listener owner. Production uses net.Listen; the
// seam lets lifecycle tests fail the listener underneath http.Server without
// conflating that path with calling the returned server closer.
func startTCPListenerWithListen(mux http.Handler, addr string, cfg *config.Config, policy tokenGatePolicy, shell webShell, auth *tcpListenerAuth, live *livePosture, listen func(network, address string) (net.Listener, error)) (func() error, tcpListenerInfo, error) {
	var token string
	var expectedToken func() (string, error)
	var expectedForRequest func(*http.Request) (string, error)
	var presentedToken func(*http.Request) string
	if auth != nil {
		// The preview listener authenticates a PER-TAB credential derived per request
		// (HMAC over the sid/tid in the path); there is no single token to advertise,
		// so the banner token stays empty and per-tab tokens are vended via
		// /v1/preview-auth.
		expectedForRequest = auth.expectedForRequest
		presentedToken = auth.presented
	} else {
		// Generate-if-absent so enabling the listener always yields a usable token;
		// the gate below re-reads the file per auth event, so rotation stays live.
		tokenPath, err := TokenPath()
		if err != nil {
			return nil, tcpListenerInfo{}, err
		}
		token, err = EnsureToken(tokenPath)
		if err != nil {
			return nil, tcpListenerInfo{}, fmt.Errorf("ensure daemon token: %w", err)
		}
		expectedToken = func() (string, error) {
			return LoadToken(tokenPath)
		}
	}
	// A plain TCP listener — net.Listen (not tls.Listen). Addr() reports the
	// concrete port even when addr requests :0 (used by the integration test).
	listener, err := listen("tcp", addr)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("bind TCP listener on %q: %w", addr, err)
	}

	// The auth + CORS posture. A live listener (the daemon's web/preview listeners,
	// #2480 PR2) reads it from config ONCE per request, so require_token /
	// require_loopback_token / cors_allowed_origins apply with no rebind; a static
	// listener (the agent-server) bakes the posture passed at bind. boundLoopback is
	// fixed at the RESOLVED bound address so a network bind never inherits
	// loopback-exempt from a config.ListenAddr that is mid-change.
	var handler http.Handler
	if live != nil {
		boundLoopback := config.IsLoopbackListenAddr(listener.Addr().String())
		handler = withLivePosture(mux, func() requestPosture {
			c := live.snapshot()
			g := &authGate{
				expectedToken:           expectedToken,
				expectedTokenForRequest: expectedForRequest,
				presentedToken:          presentedToken,
				tokenDisabled:           policy.tokenDisabled,
				loopbackExempt:          policy.loopbackExempt,
			}
			if live.policyFromConfig {
				// The control-plane web listener: token/loopback posture from live
				// config (webListenerPolicy's terms), loopback judged from the fixed
				// bound address, not the possibly-mid-change config.ListenAddr.
				g.tokenDisabled = !c.RequireToken
				g.loopbackExempt = boundLoopback && !c.RequireLoopbackToken
			}
			cors := c.CORSAllowedOrigins
			if live.previewOrigin {
				// The preview origin answers no cross-origin reader, ever — see previewOrigin.
				cors = nil
			}
			return requestPosture{gate: g, cors: cors, previewOrigin: live.previewOrigin}
		})
	} else {
		gate := &authGate{
			expectedToken:           expectedToken,
			expectedTokenForRequest: expectedForRequest,
			presentedToken:          presentedToken,
			tokenDisabled:           policy.tokenDisabled,
			loopbackExempt:          policy.loopbackExempt,
		}
		handler = withAuth(mux, gate, cfg.CORSAllowedOrigins)
	}

	// The DAEMON's TCP listener also serves the embedded browser SPA (#1592 Phase 5
	// PR2, design §1). webShellHandler serves the static shell UNAUTHENTICATED on
	// every non-/v1 path (you cannot paste a token into a page that won't load)
	// while routing /v1/... through the token gate below exactly as before. This
	// wrapper is TCP-only: the unix socket keeps its bare mux (whose `/` still
	// 404s), so the web assets never appear on the socket path. The agent-server
	// passes withoutWebShell — it cannot back the SPA (see webShell).
	switch shell {
	case withWebShell:
		handler = webShellHandler(handler)
	case previewShell:
		handler = previewShellHandler(handler)
	default: // withoutWebShell — the agent-server
		handler = noWebShellHandler(handler, noWebShellMessage)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	done := make(chan struct{})
	var closeRequested atomic.Bool
	go func() {
		defer close(done)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon TCP listener stopped: %v", err)
		}
	}()

	info := tcpListenerInfo{
		Addr:           listener.Addr().String(),
		Token:          token,
		done:           done,
		closeRequested: closeRequested.Load,
	}
	closeServer := func() error {
		// Set before Close so the Serve watcher can distinguish our teardown from
		// an underlying listener failure even when Serve returns immediately.
		closeRequested.Store(true)
		return srv.Close()
	}
	return closeServer, info, nil
}
