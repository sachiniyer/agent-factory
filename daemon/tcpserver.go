package daemon

import (
	"fmt"
	"net"
	"net/http"

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
	Addr  string // the resolved bound address (host:port, port filled in for :0)
	Token string // the bearer token clients must present
	done  <-chan struct{}
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
// It returns the STRICT zero value — token mandatory for every peer, no
// exemptions — deliberately, for the step that only opens the port. Two facts
// make that the safe default rather than a placeholder guess:
//
//   - the preview listener serves NOTHING yet (an empty mux), so nothing is
//     behind this gate to reach with or without a token; and
//   - the preview origin's real credential is a preview-scoped one wired in a
//     later step, and pre-committing to the control plane's require_token model
//     here would be the wrong answer to bake in.
//
// So the fail-safe posture holds the seam until that step replaces it, and never
// silently exposes a preview surface that does not exist.
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
type webShell bool

const (
	// withWebShell serves the browser SPA on every non-/v1 path — the daemon.
	withWebShell webShell = true
	// withoutWebShell leaves non-/v1 paths to the mux's own 404 — the agent-server.
	withoutWebShell webShell = false
)

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
// It ensures the bearer token exists before opening the port (so an operator
// enabling the listener always has a credential to present) and reads that
// token FRESH per auth event through the gate so `af token rotate` takes effect
// for new connections without a daemon restart.
func startTCPListener(mux http.Handler, addr string, cfg *config.Config, policy tokenGatePolicy, shell webShell) (func() error, tcpListenerInfo, error) {
	// Generate-if-absent so enabling the listener always yields a usable token;
	// the gate below re-reads the file per auth event, so rotation stays live.
	tokenPath, err := TokenPath()
	if err != nil {
		return nil, tcpListenerInfo{}, err
	}
	token, err := EnsureToken(tokenPath)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("ensure daemon token: %w", err)
	}
	gate := &authGate{
		expectedToken: func() (string, error) {
			return LoadToken(tokenPath)
		},
		tokenDisabled:  policy.tokenDisabled,
		loopbackExempt: policy.loopbackExempt,
	}

	// A plain TCP listener — net.Listen (not tls.Listen). Addr() reports the
	// concrete port even when addr requests :0 (used by the integration test).
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("bind TCP listener on %q: %w", addr, err)
	}

	// The DAEMON's TCP listener also serves the embedded browser SPA (#1592 Phase 5
	// PR2, design §1). webShellHandler serves the static shell UNAUTHENTICATED on
	// every non-/v1 path (you cannot paste a token into a page that won't load)
	// while routing /v1/... through the token gate below exactly as before. This
	// wrapper is TCP-only: the unix socket keeps its bare mux (whose `/` still
	// 404s), so the web assets never appear on the socket path. The agent-server
	// passes withoutWebShell — it cannot back the SPA (see webShell).
	handler := withAuth(mux, gate, cfg.CORSAllowedOrigins)
	if shell == withWebShell {
		handler = webShellHandler(handler)
	} else {
		handler = noWebShellHandler(handler)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon TCP listener stopped: %v", err)
		}
	}()

	info := tcpListenerInfo{
		Addr:  listener.Addr().String(),
		Token: token,
		done:  done,
	}
	return srv.Close, info, nil
}
