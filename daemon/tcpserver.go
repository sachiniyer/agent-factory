package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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
//     gate CAN front a network listener, but only when the operator opted in:
//     #5137 makes that combination a refused bind by default (the daemon starts
//     and the unix sockets still work; the TCP listener stays unbound), and
//     allow_unauthenticated_network=true is the explicit release — which is when
//     this policy actually gets to run tokenless on the network. Nothing below
//     authenticates such a peer — that is the configuration doing exactly what
//     it says.
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
	// posturePair, when set, replaces snapshot: it returns the config AND the
	// #5183 upgrade-probation token floor in ONE atomic read. The apply sequence
	// is three ordered writes — arm, config publish, disarm — and reading the
	// floor as a separate atomic from the config lets a request straddle them:
	// old tokenless config observed before the publish, cleared floor observed
	// after it → the phantom pair admits the request unauthenticated on a socket
	// both real snapshots would have gated. Pairing is the only answer — neither
	// read order alone closes both the arm and the disarm straddle. Nil means
	// (snapshot(), false) — no floor.
	posturePair func() (*config.Config, bool)
	// freeze is this listener KIND's refusal freeze, shared by every generation:
	// a refusal can land while an OLDER generation is still draining after a
	// normal rebind, so per-generation state cannot cover the hole — the freeze
	// must reach every socket the kind is still serving (#5137). Nil for
	// postures no refusal can retire (the preview origin, test stand-ins).
	freeze *postureFreeze
	// seq is this generation's issuance order within its kind. A freeze covers
	// exactly the generations issued before it (seq <= freeze.through), so a
	// listener bound AFTER a refusal reads live: it was admitted under a newer
	// config the posture allowed, while an earlier generation keeps gating on
	// the config it was serving when the refusal landed.
	seq uint64
	// policyFromConfig derives the gate's token/loopback posture from the snapshot
	// (require_token / require_loopback_token) — the control-plane web listener. The
	// preview listener leaves it false: its gate posture is the fixed strict zero
	// value (previewListenerPolicy).
	policyFromConfig bool
	// sandboxTokens admits per-session sandbox callback credentials (#2999). Set
	// ONLY by the control-plane web listener: the preview origin carries no
	// control plane, so a sandbox credential must never authenticate there.
	sandboxTokens *sandboxTokenRegistry
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
	// previewWarmingUp reports whether the daemon is still restoring, so a denial on
	// the preview origin can render the RETRYING notice instead of the terminal one.
	previewWarmingUp func() bool
}

// postureFreeze pins the serving posture of ONE listener kind across all its
// generations (#5137). Retire's drain deliberately lets in-flight handlers
// finish, and a keep-alive conn can dispatch a request that reaches the gate
// AFTER the refusing config has published — without the pin that request reads
// tokenDisabled and reaches the control API unauthenticated on a socket that
// was just refused. Generations are the point: a normal rebind leaves the old
// listener draining, and a refusal arriving while that drain runs must cover
// it, not just the handle bound right now.
//
// pins holds the freeze history as ascending `through` ranges. Each pin
// covers exactly the generations issued since the previous pin — (lastThrough,
// through] — frozen to the config they were serving at that moment. FIRST
// COVERAGE WINS, and that is a security rule, not an implementation detail:
// a generation keeps the pin that first covered it, so a later apply can
// never weaken an already-draining generation's gate by re-pinning it onto a
// posture that relaxed further (#5137 review — the single-cfg shape let a
// refusal following an opt-in apply move an older tokened generation onto the
// tokenless config the new one legitimately served).
type postureFreeze struct {
	mu   sync.Mutex
	pins []frozenRange
}

// posturePinHorizon bounds how long a pin's coverage can still be consulted.
// A pin exists to carry a generation through its retire drain (~seconds);
// past that window its livePosture is never reached again, and nothing else
// would ever reclaim the range — every relax-or-refuse apply would otherwise
// append a config snapshot forever and grow the mutex'd scan on the request
// admission path without bound (#5137 review). A minute is tens of multiples
// of the drain window, so expiry can never cut a live drain — only ranges
// whose generations are provably finished draining.
var posturePinHorizon = time.Minute

// frozenRange freezes generations (prev, through] to cfg — the config they
// were serving when this pin landed. pinnedAt records when the pin landed so
// coverage expires once every generation it could still serve has certainly
// finished draining.
type frozenRange struct {
	cfg      *config.Config
	through  uint64
	pinnedAt time.Time
}

// pin freezes the generations issued since the last pin, up to through, onto
// cfg — the config they were serving. Called only by refusal-driven retires
// and relaxation applies: a posture change the operator was allowed to write
// keeps the ordinary live-read semantics otherwise. Three no-ops: cfg==nil (a
// refusal without a serving config has nothing to preserve), a cfg that is
// ITSELF refused (a refused posture was never legitimately served — pinning
// it would hand draining sockets the very gate the freeze exists to keep
// out), and a through that adds no coverage (through==0 covers nothing —
// generations number from 1 — and a through at or below the last pin's names
// only already-frozen generations, whose first coverage wins).
func (f *postureFreeze) pin(cfg *config.Config, through uint64) {
	if cfg == nil || through == 0 || config.ListenerBindRefusal(cfg) != "" {
		return
	}
	now := time.Now()
	f.mu.Lock()
	f.pruneLocked(now)
	if n := len(f.pins); n == 0 || through > f.pins[n-1].through {
		f.pins = append(f.pins, frozenRange{cfg: cfg, through: through, pinnedAt: now})
	}
	f.mu.Unlock()
}

// pruneLocked drops ranges past the pin horizon — every generation they could
// still serve finished its retire drain long ago, so nothing consults them.
// Dropping is safe precisely because expiry implies dead readers: after the
// settle unfreeze no still-serving generation is ever covered, and a drained
// generation's livePosture is unreachable — remapping such a generation onto
// a later range can never be observed. Caller holds f.mu.
func (f *postureFreeze) pruneLocked(now time.Time) {
	keep := f.pins[:0]
	for _, r := range f.pins {
		if now.Sub(r.pinnedAt) <= posturePinHorizon {
			keep = append(keep, r)
		}
	}
	f.pins = keep
}

// unfreezeFrom lifts the freeze for every generation numbered seq or higher —
// the apply settled with that generation still bound, so it returns to the
// live read. Coverage is first-win and ranges ascend, so only ranges reaching
// >= seq need touching: truncate the last range at seq-1, drop it if that
// empties it under the previous range's coverage, and repeat — earlier ranges
// always end below the last one's start, so the walk terminates at the first
// range that stops short of seq.
func (f *postureFreeze) unfreezeFrom(seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(time.Now())
	for len(f.pins) > 0 {
		last := &f.pins[len(f.pins)-1]
		if last.through < seq {
			return
		}
		last.through = seq - 1
		var prev uint64
		if len(f.pins) > 1 {
			prev = f.pins[len(f.pins)-2].through
		}
		if last.through <= prev {
			f.pins = f.pins[:len(f.pins)-1]
			continue
		}
		return
	}
}

// current returns the config this listener evaluates requests under — the
// serving config of the first pin that covered this generation, OVERLAID with
// the live config's token requirements where those are stricter, or the live
// snapshot when no pin reaches it. The overlay keeps the #2480 direction
// honest through a freeze: a tightening apply still reaches draining conns on
// a frozen generation — only relaxations are pinned out. The probation token
// floor (#5183) overlays last, on whichever config that chose, in the same
// OR-only direction.
func (l *livePosture) current() *config.Config {
	var live *config.Config
	floored := false
	if l.posturePair != nil {
		live, floored = l.posturePair()
	} else {
		live = l.snapshot()
	}
	cfg := live
	if f := l.freeze; f != nil {
		f.mu.Lock()
		f.pruneLocked(time.Now())
		for _, r := range f.pins {
			if l.seq <= r.through {
				cfg = tightenFrozenPosture(r.cfg, live)
				break
			}
		}
		f.mu.Unlock()
	}
	if floored {
		cfg = withProbationTokenFloor(cfg)
	}
	return cfg
}

// withProbationTokenFloor overlays the #5183 upgrade-probation floor onto the
// config a request is gated under: an upgrade candidate keeping a journaled
// socket bound demands the bearer token whatever the loaded config's
// require_token says. Same direction as tightenFrozenPosture — the floor ORs
// into the token requirement and never relaxes anything. It raises
// RequireToken only: the floor exists so a kept-bound NETWORK socket never
// answers unauthenticated, and require_loopback_token governs only a
// loopback-bound socket (the gate's loopbackExempt is judged from the bound
// address), which is not that exposure. Allocates only while the floor is
// armed and the config is tokenless — the probation window, never steady
// state.
func withProbationTokenFloor(cfg *config.Config) *config.Config {
	if cfg == nil || cfg.RequireToken {
		return cfg
	}
	floored := *cfg
	floored.RequireToken = true
	return &floored
}

// tightenFrozenPosture merges a pinned config with the live one field-wise so
// a later apply that TIGHTENS the gate still reaches frozen generations
// (#5137 review — first coverage preserves the pin's posture, but a pin is a
// floor, not a ceiling). RequireToken and RequireLoopbackToken are the gate's
// token fields and true is the stricter direction for both, so the merge is a
// plain OR. CORSAllowedOrigins is an allow-list — stricter means SMALLER — so
// its merge is the intersection: an origin survives only when the pin AND the
// live config both allow it, which lets a live removal through without ever
// widening a frozen gate past its pinned allow-list. AllowedHosts (the Host
// guard's allow-list, read with the same posture) merges the same way. The
// merge allocation is skipped when nothing tightened — the pinned object
// itself is returned so the common frozen path allocates nothing per request.
func tightenFrozenPosture(pinned, live *config.Config) *config.Config {
	if pinned == nil {
		return live
	}
	if live == nil || (pinned.RequireToken == live.RequireToken &&
		pinned.RequireLoopbackToken == live.RequireLoopbackToken &&
		slices.Equal(pinned.CORSAllowedOrigins, live.CORSAllowedOrigins) &&
		slices.Equal(pinned.AllowedHosts, live.AllowedHosts)) {
		return pinned
	}
	merged := *pinned
	merged.RequireToken = merged.RequireToken || live.RequireToken
	merged.RequireLoopbackToken = merged.RequireLoopbackToken || live.RequireLoopbackToken
	merged.CORSAllowedOrigins = intersectCORS(pinned.CORSAllowedOrigins, live.CORSAllowedOrigins)
	// network.allowed_hosts is an allow-list too, so the same intersection: a live
	// removal reaches a frozen generation, a live addition never widens it.
	merged.AllowedHosts = intersectCORS(pinned.AllowedHosts, live.AllowedHosts)
	return &merged
}

// intersectCORS keeps an origin only when BOTH allow-lists name it — the
// pin's list is the floor and live can only shrink it. Live order is
// preserved so the merged list reads the way the operator last wrote it. An
// empty list on either side is deny-all, which an intersection inherits.
func intersectCORS(pinned, live []string) []string {
	if len(pinned) == 0 || len(live) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(pinned))
	for _, o := range pinned {
		allowed[o] = struct{}{}
	}
	var out []string
	for _, o := range live {
		if _, ok := allowed[o]; ok {
			out = append(out, o)
		}
	}
	return out
}

// connTracker tracks the HIJACKED connections — WebSocket upgrades — of one
// listener KIND across its generations (#5137). http.Server forgets a conn the
// moment it is hijacked, so neither Shutdown's drain nor Close reaches it; a
// stream opened on listener generation A survives A's retirement and outlives
// the handle that owned it. The tracker therefore lives on the listener owner
// (webListeners), not the handle: a later POLICY retire — a refusal — must be
// able to name every hijacked conn the kind is still serving, not only the ones
// the current generation admitted.
//
// Membership is two-way: ConnState adds a conn when it hijacks, and the wrapped
// conn removes itself on Close (net/http never reports a state for a hijacked
// conn again — without the self-removal every finished stream would stay
// strongly referenced for the daemon's life). Severing is a generation
// WATERMARK, not a shared latch: sever() retires every generation allocated so
// far, so a conn whose handler hijacks LATE — its upgrade passed the old gate
// before the retire, and http.Server reports StateHijacked only after — dies
// on arrival even if a new generation has since bound. A boolean latch cannot
// express that: the new generation's begin() would lift the severing that the
// late-arriving retired-generation conn still needs to hit.
type connTracker struct {
	mu             sync.Mutex
	nextGen        uint64
	severedThrough uint64
	conns          map[net.Conn]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: make(map[net.Conn]struct{})}
}

// begin allocates the generation a freshly bound listener belongs to; its ID is
// closed over by that server's ConnState. Generations are monotonic, and
// sever() retires every ID ≤ severedThrough — so a generation created before
// the sever is dead no matter when its stragglers hijack, while the next bind's
// generation numbers above the watermark and is unaffected by it.
func (t *connTracker) begin() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextGen++
	return t.nextGen
}

// track records a conn the server just handed to a hijacking handler, or closes
// it on the spot when its generation is dead. The close runs outside mu so
// trackedConn.Close's self-removal cannot re-enter the lock.
func (t *connTracker) track(gen uint64, c net.Conn) {
	t.mu.Lock()
	dead := gen <= t.severedThrough
	if !dead {
		t.conns[c] = struct{}{}
	}
	t.mu.Unlock()
	if dead {
		_ = c.Close()
	}
}

// untrack drops a conn — called by trackedConn.Close, so a finished stream
// releases its slot rather than growing the set for the daemon's life.
func (t *connTracker) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

// sever force-closes every tracked conn now and marks every generation that has
// ever begun as dead — a late hijack from a retired listener's still-running
// handler is then refused on arrival even after a new generation binds.
// retire() drains in-flight NORMAL requests — the refusal's own reply still
// reaches its client — while a hijacked stream has no reply left to give: it
// only lives to keep serving a posture the socket must stop serving. Closes
// run outside mu for the same re-entrancy reason as track.
func (t *connTracker) sever() {
	t.mu.Lock()
	t.severedThrough = t.nextGen
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	clear(t.conns)
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// trackedListener wraps the raw listener so every accepted conn is a trackedConn
// — the one reliable hook for "this conn is gone" that exists for hijacked conns.
type trackedListener struct {
	net.Listener
	tracker *connTracker
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: c, tracker: l.tracker}, nil
}

// trackedConn removes itself from the tracker on Close — the ONLY teardown event
// a hijacked conn still reports, since http.Server's ConnState is silent for it
// after StateHijacked.
type trackedConn struct {
	net.Conn
	untrackOnce sync.Once
	tracker     *connTracker
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.untrackOnce.Do(func() { c.tracker.untrack(c) })
	return err
}

// startTCPListener binds the plain-HTTP TCP listener on addr and serves mux
// wrapped in a token-enforcing gate + the CORS allow-list. It returns the
// listener's teardown handle (tcpListenerHandle: close now, or retire and drain)
// and the banner payload the caller logs.
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
func startTCPListener(mux http.Handler, addr string, cfg *config.Config, policy tokenGatePolicy, shell webShell, auth *tcpListenerAuth, live *livePosture) (*tcpListenerHandle, tcpListenerInfo, error) {
	return startTCPListenerWithListen(mux, addr, cfg, policy, shell, auth, live, nil, net.Listen)
}

// startTCPListenerWithListen is startTCPListener with the socket constructor
// supplied by the restartable-listener owner. Production uses net.Listen; the
// seam lets lifecycle tests fail the listener underneath http.Server without
// conflating that path with tearing the server down through the returned handle.
// hijacked, when non-nil, is the listener KIND's connTracker (see above): the
// listener is wrapped so conns self-unregister, and ConnState reports each
// hijack into it. Nil means no hijack tracking — the preview and agent-server
// listeners have no policy retire that would ever need one.
func startTCPListenerWithListen(mux http.Handler, addr string, cfg *config.Config, policy tokenGatePolicy, shell webShell, auth *tcpListenerAuth, live *livePosture, hijacked *connTracker, listen func(network, address string) (net.Listener, error)) (*tcpListenerHandle, tcpListenerInfo, error) {
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
	// build is the live listener's once-per-request posture read, kept so the Host
	// guard below can make that read itself and share it (withLiveHostGuard).
	var build func() requestPosture
	if live != nil {
		boundLoopback := config.IsLoopbackListenAddr(listener.Addr().String())
		build = func() requestPosture {
			c := live.current()
			g := &authGate{
				expectedToken:           expectedToken,
				expectedTokenForRequest: expectedForRequest,
				presentedToken:          presentedToken,
				tokenDisabled:           policy.tokenDisabled,
				loopbackExempt:          policy.loopbackExempt,
				sandboxTokens:           live.sandboxTokens,
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
			return requestPosture{gate: g, cors: cors, allowedHosts: c.AllowedHosts,
				previewOrigin: live.previewOrigin, previewWarmingUp: live.previewWarmingUp}
		}
		handler = withLivePosture(mux, build)
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
	//
	// Each shell also decides the listener's Host rule (hostguard.go), outermost so
	// it runs before the static shell, every /v1 route and every WS upgrade:
	//   - withWebShell, the control plane, answers only Hosts that name this daemon:
	//     loopback names, the bind address's own host, the address the client
	//     reached, and network.allowed_hosts — read with the live posture.
	//   - previewShell keeps its own stricter rule: previewShellHandler serves only
	//     af-minted *.localhost labels and 404s every other Host before the gate.
	//   - withoutWebShell (the agent-server) has none: its token is mandatory for
	//     every peer and it serves a browser nothing without one.
	switch shell {
	case withWebShell:
		handler = webShellHandler(handler)
		listenHost := config.ListenAddrHostName(addr)
		label := hostGuardListenerLabel(listener.Addr().String())
		if build != nil {
			handler = withLiveHostGuard(handler, label, listenHost, build)
		} else {
			allowed := cfg.AllowedHosts
			handler = withHostGuard(handler, label, listenHost, func() []string { return allowed })
		}
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
	if hijacked != nil {
		// http.Server stops tracking a connection the moment it is hijacked,
		// which is what makes a policy retire incomplete without sever: the
		// tracker must be able to name the conns Shutdown and Close cannot.
		// The generation is allocated BEFORE Serve accepts — this listener's
		// conns tag it at hijack time so a sever that predates a straggler's
		// upgrade still reaches it.
		gen := hijacked.begin()
		listener = &trackedListener{Listener: listener, tracker: hijacked}
		srv.ConnState = func(c net.Conn, st http.ConnState) {
			if st == http.StateHijacked {
				hijacked.track(gen, c)
			}
		}
	}
	h := &tcpListenerHandle{srv: srv, ln: listener, addr: listener.Addr().String()}
	go func() {
		defer close(done)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon TCP listener stopped: %v", err)
		}
	}()

	info := tcpListenerInfo{
		Addr:           h.addr,
		Token:          token,
		done:           done,
		closeRequested: h.closeRequested.Load,
	}
	return h, info, nil
}

// listenerRetireGrace bounds how long a RETIRED listener may spend draining the
// requests already in flight on it before those connections are force-closed
// (#3722). Two seconds is generous for a control-plane reply that is already
// written and short enough that a client which never drains cannot pin the old
// server — and its connections, and the port — indefinitely.
//
// A var, not a const, so the deadline test can shorten it; nothing in production
// writes it. retire() reads it on the CALLER's goroutine and hands the value to
// the drain — see the capture there for why the drain must not read it itself.
var listenerRetireGrace = 2 * time.Second

// tcpListenerHandle owns the teardown of one bound TCP listener. It offers two
// ways out, and choosing between them is the whole of #3722:
//
//   - close() cuts everything immediately, including a request mid-handler. That
//     is right at DAEMON SHUTDOWN, where the process is going away and there is
//     nothing left to flush a reply into.
//   - retire() is right for a listener the daemon is REPLACING while it keeps
//     running. A config write that moves network.listen_addr arrives ON the
//     listener it moves, so closing that listener from inside the handler
//     destroys the connection the 200 was about to be written to: the operator is
//     told a committed, applied write failed, and retries against an address the
//     daemon has already left.
//
// retire() splits along the one line that matters — which half of a shutdown can
// afford to wait:
//
//   - Everything that does NOT wait happens synchronously: the listener stops
//     accepting and every connection that is idle right now is dropped. So a
//     retired address refuses new connections, and serves nothing further on a
//     pooled keep-alive one, before the caller returns — the property this path
//     has had since #2480 PR2. It is a BOUND, not a speedup: a variant that
//     spawned this half as a goroutine too still passed every listener test here,
//     because the goroutine wins that race in practice. What it cannot promise is
//     winning it on a box under load, and "the old address answered a new client
//     after the daemon moved off it" is not a window worth leaving open for the
//     sake of one less line.
//   - WAITING for the requests still in flight is asynchronous, because it must
//     be: Shutdown waits for in-flight handlers to return, and the handler that
//     triggered the rebind is one of them, so waiting on the caller's goroutine
//     would deadlock it against itself.
type tcpListenerHandle struct {
	srv *http.Server
	// ln is the socket srv serves, kept so the teardowns below can close it
	// THEMSELVES. http.Server closes only the listeners its Serve goroutine has
	// already registered (trackListener) — and Serve is spawned, not started, at
	// bind time, so a retire/close that lands before the goroutine is scheduled
	// frees nothing: the socket stays bound until Serve runs and its own
	// deferred close fires. That is a scheduling delay, not a bound — on a
	// loaded macOS runner it outlasted two bind attempts, so a same-port move's
	// post-release bind met the socket retire() had supposedly freed, and the
	// rollback met it too (#5140 darwin). close()/retire() therefore close ln
	// directly, on the caller's goroutine — the guarantee the release path of
	// listener_rebind.go stands on. Double-closing is safe: Serve wraps the
	// socket in onceCloseListener and a second Close on the raw listener is a
	// reported-then-ignored error.
	ln net.Listener
	// addr is the resolved bound address, snapshotted at bind: it is read for the
	// deadline warning after the listener has been closed.
	addr string
	// closeRequested records that a teardown was ASKED FOR, so the Serve watcher
	// can tell one from an underlying listener failure. Set before either teardown
	// touches the socket, so it is already true when Serve returns.
	closeRequested atomic.Bool
	// retired records that the teardown was a RETIREMENT rather than a close.
	// Both set closeRequested, and once a drain has finished the two are
	// indistinguishable from outside — the server is down either way — so this is
	// what lets a test pin WHICH teardown a call site chose. That question has to
	// be answerable directly: the observable difference (a connection surviving
	// the swap) is only observable while http.Server still counts that connection
	// as in flight, which for a half-written request means the first five seconds
	// of its life, and a test that raced that window failed on a loaded runner
	// for a reason that had nothing to do with the code under test.
	retired    atomic.Bool
	retireOnce sync.Once
}

// close tears the listener down immediately: every accepted connection is cut,
// in-flight handlers included. Daemon shutdown only — for a listener being
// replaced while the daemon keeps running, see retire.
func (h *tcpListenerHandle) close() error {
	// Set before Close so the Serve watcher can distinguish our teardown from
	// an underlying listener failure even when Serve returns immediately.
	h.closeRequested.Store(true)
	err := h.srv.Close()
	// srv.Close reaches only listeners the spawned Serve goroutine has already
	// registered; a not-yet-scheduled Serve leaves the socket bound until its
	// deferred close runs (the ln field comment). Close it on THIS goroutine —
	// ordering after the srv call so an Accept woken by the close sees
	// inShutdown and returns ErrServerClosed rather than a raw accept error.
	if h.ln != nil {
		_ = h.ln.Close()
	}
	return err
}

// retire stops accepting now and drains what is already in flight off the
// caller's goroutine (#3722), so the reply to the very request that retired this
// listener still reaches its client. Idempotent.
func (h *tcpListenerHandle) retire() {
	h.retireOnce.Do(func() {
		h.closeRequested.Store(true)
		h.retired.Store(true)
		// The synchronous half, and Shutdown itself is what performs it: an
		// ALREADY-EXPIRED deadline runs its close-listeners + close-idle-connections
		// prologue and then returns instead of waiting — measured on go1.25, and the
		// order is the documented one ("first closing all open listeners, then
		// closing all idle connections, and then waiting"). Two things follow that a
		// bare listener.Close() would not give: idle keep-alive connections to the
		// retired address stop serving here rather than whenever the goroutine below
		// is scheduled, and Serve returns ErrServerClosed rather than a raw accept
		// error, so a deliberate retirement logs no "listener stopped" warning.
		//
		// Promptness is all that rides on the prologue order: if a future runtime
		// returned before closing idle connections, drain() closes them a moment
		// later anyway.
		expired, cancel := context.WithDeadline(context.Background(), time.Now())
		_ = h.srv.Shutdown(expired)
		cancel()
		// Shutdown closes only listeners the spawned Serve goroutine has already
		// registered, so a retire landing before Serve is scheduled would leave
		// the port bound for a scheduling delay the caller cannot see (the ln
		// field comment). Close the socket HERE so the port is free when retire
		// returns — the bound the same-port release path stands on. Ordering is
		// after Shutdown for the same reason as in close(): inShutdown is already
		// set, so a woken Accept returns ErrServerClosed, not a raw error.
		if h.ln != nil {
			_ = h.ln.Close()
		}
		// The grace is captured BEFORE the goroutine starts — the rule
		// boundedRecordRootAbsent already states in deleteproject_target.go.
		// Reading the package var inside the goroutine races a test that swaps it,
		// because the goroutine outlives the call it was spawned for, which is the
		// whole point of the asynchronous half. #3772 is exactly that: a drain
		// leaked by one test read the var while the next test's withRetireGrace
		// wrote it, and -race failed a PR that touched neither.
		grace := listenerRetireGrace
		go h.drain(grace)
	})
}

// drain waits for the requests already in flight on a retired listener, then
// closes it. grace is the deadline retire() captured for it; it is a parameter
// rather than a read of listenerRetireGrace because this runs on a goroutine that
// outlives its caller (#3772).
//
// The deadline is the ONLY thing that justifies the force close, so it is what
// the branch tests for rather than err != nil. Shutdown has a second way to
// return non-nil here — it reports the error from closing the listener retire's
// synchronous half already closed — and while that particular one arrives only
// once everything is idle anyway, "any error means the client stalled" is the
// wrong rule to leave lying next to a srv.Close().
func (h *tcpListenerHandle) drain(grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := h.srv.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
		// A client that never drains its reply must not keep the retired server —
		// and the connections it holds — alive for as long as it likes. Say so:
		// this is the one outcome where a reply was genuinely lost. It names the
		// CAPTURED grace, because that is the deadline that actually fired — a
		// re-read of the package var could name one this drain never used.
		log.WarningLog.Printf("retired listener on %s still had connections in flight after %s: closing them", h.addr, grace)
		_ = h.srv.Close()
	}
}
