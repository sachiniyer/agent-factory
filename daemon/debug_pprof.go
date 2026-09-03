package daemon

import (
	"net/http"
	"net/http/pprof"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The opt-in runtime profiling endpoint (#3651), served at
// GET /v1/debug/pprof/{profile} by stdlib net/http/pprof — no new dependency, and
// the endpoint set is whatever that package already exposes (heap, goroutine,
// allocs, profile, block, mutex, trace, threadcreate, plus cmdline and symbol).
//
// A heap or goroutine profile is a dump of live process memory, which on this
// daemon holds session titles, worktree paths, and prompt text. Two properties
// follow from that, and both are enforced here rather than described:
//
//   - UNIX SOCKET ONLY. startHTTPServer wraps ONLY the unix listener's handler in
//     withDebugPprof; the shared mux it hands newWebListeners is the unwrapped one,
//     so the routes do not exist on the listen_addr / preview_listen_addr listeners
//     at all. This is structural, not a peer check: there is no request on the
//     network listener that could reach a pprof handler, with any token, on any
//     interface, because no such handler is registered there. The socket's own 0600
//     permissions are then the whole authentication, exactly as for every other
//     route it carries (#1029).
//   - OFF BY DEFAULT. With the opt-in absent, withDebugPprof returns the mux
//     UNCHANGED, so /v1/debug/pprof/heap lands on the mux catch-all and answers the
//     ordinary 404 unknown-route envelope — the same answer as /v1/Nope. A daemon
//     that was not started with profiling on is indistinguishable from one built
//     before this existed, which is the point: a disabled-looking 403 (or an empty
//     200) advertises a surface to probe for.
//
// Sampling rates for the block and mutex profiles are deliberately NOT set. Both
// cost something to collect continuously, and Go leaves them at 0 (off), so those
// two endpoints answer an empty profile until an operator turns the rate on
// separately. Enabling the ENDPOINT therefore adds no steady-state cost.

// debugPprofEnv is the process-level opt-in, for a daemon started by hand or from
// a unit file whose environment is easier to edit than the global config. Config
// is the persistent switch; this is the one-run override.
const debugPprofEnv = "AF_DEBUG_PPROF"

// stdlibPprofPrefix is the path net/http/pprof serves itself on. pprof.Index
// resolves WHICH profile to serve by cutting exactly this prefix off the request
// path, so the mount point below has to be stripped before a request reaches it —
// otherwise every profile request renders the index page instead of the profile.
const stdlibPprofPrefix = "/debug/pprof/"

// debugPprofMount is where that stdlib layout is mounted on the daemon's socket.
// /v1 is the path #3651 pinned, and it keeps the socket's surface uniform with
// every other route it carries. It also decides what the NETWORK listeners answer
// for these paths: a /v1 path reaches the shared mux and gets its unknown-route
// envelope, where a path outside /v1 would be swallowed by the SPA shell and
// answer index.html with a 200 (#2928, mux_prefix_test.go).
const debugPprofMount = "/v1"

// debugPprofPrefix is the served path prefix — /v1/debug/pprof/. Derived rather
// than spelled out so the served path and the stripped mount cannot disagree.
const debugPprofPrefix = debugPprofMount + stdlibPprofPrefix

// debugPprofEnabled reports whether this daemon serves the profiling endpoint.
// The config key is the persistent switch and AF_DEBUG_PPROF overrides it for one
// process, mirroring the auto-update opt-out contract (internal/autoupdate): a
// valid environment value wins, an invalid one is named in the log and leaves the
// config value in force.
//
// It is read ONCE, at startHTTPServer, from the frozen startup config — the key is
// classified EffectNextDaemonStart (config/effect.go) because the route table is
// built at bind time, so a later `af config set` is honestly reported as waiting
// for the next daemon start rather than silently ignored.
func debugPprofEnabled(cfg *config.Config) bool {
	enabled := false
	if cfg != nil {
		enabled = cfg.DebugPprof
	}
	raw, ok := os.LookupEnv(debugPprofEnv)
	if !ok {
		return enabled
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	case "":
		return enabled
	default:
		log.WarningLog.Printf("debug pprof: ignoring invalid %s=%q (expected true/false, 1/0, yes/no, on/off)", debugPprofEnv, raw)
		return enabled
	}
}

// withDebugPprof mounts the stdlib profiling handlers at debugPprofPrefix in front
// of next, or returns next untouched when profiling is off.
//
// Returning next ITSELF when disabled — rather than a wrapper that 404s the prefix
// — is what makes the off state indistinguishable from a daemon that never had the
// feature: the request falls through to the mux catch-all and gets its unknown-route
// envelope, with no branch anywhere that could answer differently.
//
// Only startHTTPServer's unix listener is wrapped. Passing the shared mux here
// would put the routes on the TCP listeners too, since both listeners serve one mux
// (httpserver.go) — so this returns a NEW handler and the caller keeps the mux for
// the network side rather than mutating it.
func withDebugPprof(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	// Registered on the stdlib's own paths, then mounted under /v1 by stripping
	// that segment: pprof.Index cuts stdlibPprofPrefix off the path to pick the
	// profile, so it only resolves a named profile when it sees its own layout.
	debug := http.NewServeMux()
	debug.HandleFunc(stdlibPprofPrefix, pprof.Index)
	debug.HandleFunc(stdlibPprofPrefix+"cmdline", pprof.Cmdline)
	debug.HandleFunc(stdlibPprofPrefix+"profile", pprof.Profile)
	debug.HandleFunc(stdlibPprofPrefix+"symbol", pprof.Symbol)
	debug.HandleFunc(stdlibPprofPrefix+"trace", pprof.Trace)
	mounted := http.StripPrefix(debugPprofMount, debug)

	// Prefix match, so the bare /v1/debug/pprof (no trailing slash) falls through
	// to the mux catch-all rather than redirecting. The index lives at the prefix
	// WITH the slash, which is the path the doc and the profile links both use.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, debugPprofPrefix) {
			mounted.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
