package daemon

import (
	"fmt"
	"net"
	"net/http"
	"strings"

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
}

// tokenGatePolicy is how a TCP listener's bearer-token gate treats peers. Its
// zero value is the strict, fail-safe posture — token mandatory for every peer,
// no exemptions — so a caller opts INTO relaxations explicitly (#1696):
//
//   - the daemon's own listen_addr web listener passes {loopbackExempt: true}
//     (and tokenDisabled from require_token=false) so a same-machine browser
//     needs no token while network peers still do;
//   - the agent-server passes the zero value, keeping its token mandatory for
//     every peer (it exists to be reached over the network — the token must
//     never be optional there).
type tokenGatePolicy struct {
	// tokenDisabled drops the token for ALL peers (require_token=false).
	tokenDisabled bool
	// loopbackExempt lets 127.0.0.1/::1 peers skip the token.
	loopbackExempt bool
}

// isLoopbackListenAddr reports whether a listen_addr binds ONLY the loopback
// interface (127.0.0.1 / ::1 / localhost). It governs the loopback token
// exemption, and the distinction is load-bearing for security now that the
// recommended way to add TLS is a same-host reverse proxy:
//
// A reverse proxy on the same host (nginx/Caddy terminating TLS) connects to the
// daemon from 127.0.0.1, so EVERY request it forwards has a loopback RemoteAddr —
// indistinguishable from a genuine same-machine user. If the loopback exemption
// applied on a network-bound listener, all proxied traffic would skip the token
// and reach the control plane unauthenticated. So the exemption is safe ONLY when
// the listener itself is loopback-bound (where a loopback RemoteAddr truly is a
// local peer). On a NETWORK bind (0.0.0.0, a routable/Tailscale IP, or an empty
// host = every interface) the exemption is withheld and the token is required for
// all peers, loopback-origin included. An unparseable address fails safe to "not
// loopback" (token enforced).
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "" {
		return false // empty host binds every interface — network-reachable
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webListenerPolicy is the token-gate posture for the daemon's own listen_addr
// web listener, derived from config. It relaxes the fail-safe default in exactly
// two ways, and the loopback relaxation is now bind-aware:
//
//   - tokenDisabled from require_token=false — drop the token for ALL peers on a
//     network the operator fully trusts (Tailscale/VPN). Unchanged.
//   - loopbackExempt lets same-machine peers skip the token, BUT only when the
//     listener is LOOPBACK-BOUND. On a network bind the exemption is withheld
//     regardless of require_loopback_token: a same-host reverse proxy connects
//     from 127.0.0.1, so exempting loopback there would let anything behind the
//     proxy reach the control plane with no token. require_loopback_token=true
//     withdraws the exemption even on a loopback bind (shared/multi-user host).
//
// The agent-server does NOT use this — it passes the strict zero-value policy
// (token mandatory for every peer) directly.
func webListenerPolicy(cfg *config.Config) tokenGatePolicy {
	return tokenGatePolicy{
		tokenDisabled:  !cfg.RequireToken,
		loopbackExempt: isLoopbackListenAddr(cfg.ListenAddr) && !cfg.RequireLoopbackToken,
	}
}

// startTCPListener binds the plain-HTTP TCP listener on cfg.ListenAddr and
// serves mux wrapped in a token-enforcing gate + the CORS allow-list. It
// returns a cleanup function that shuts the server down and the banner payload
// the caller logs. policy selects how the gate treats peers (loopback
// exemption / token disable); its zero value is the strict "token mandatory for
// everyone" posture.
//
// It ensures the bearer token exists before opening the port (so an operator
// enabling the listener always has a credential to present) and reads that
// token FRESH per auth event through the gate so `af token rotate` takes effect
// for new connections without a daemon restart.
func startTCPListener(mux http.Handler, cfg *config.Config, policy tokenGatePolicy) (func() error, tcpListenerInfo, error) {
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
	// concrete port even when cfg.ListenAddr requests :0 (used by the
	// integration test).
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, tcpListenerInfo{}, fmt.Errorf("bind TCP listener on %q: %w", cfg.ListenAddr, err)
	}

	// The TCP listener also serves the embedded browser SPA (#1592 Phase 5 PR2,
	// design §1). webShellHandler serves the static shell UNAUTHENTICATED on every
	// non-/v1 path (you cannot paste a token into a page that won't load) while
	// routing /v1/... through the token gate below exactly as before. This wrapper
	// is TCP-only: the unix socket keeps its bare mux (whose `/` still 404s), so
	// the web assets never appear on the socket path.
	srv := &http.Server{
		Handler:           webShellHandler(withAuth(mux, gate, cfg.CORSAllowedOrigins)),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon TCP listener stopped: %v", err)
		}
	}()

	info := tcpListenerInfo{
		Addr:  listener.Addr().String(),
		Token: token,
	}
	return srv.Close, info, nil
}
