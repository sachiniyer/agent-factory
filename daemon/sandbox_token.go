package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
)

// The sandbox callback credential (#2999).
//
// A provisioned sandbox can be driven BY the daemon but could not call back INTO
// it at all. Giving it the operator's own token would have been the easy answer
// and the wrong one: that token grants the whole control plane including
// DeliverPrompt, which runs instructions through every agent on the machine — so
// one compromised sandbox would own the host.
//
// What a sandbox gets instead is a token that is:
//
//   - PER SESSION, so it can be revoked with that session and names who used it;
//   - SCOPED, and at present to a single route — SuggestSessionName — with every
//     other route denied unless it opts in (see HTTPRoute.sandboxAllowed, which
//     carries the rule and the reasoning for each denial). Notably NOT creating a
//     session: that names no session and still starts an agent on the host with a
//     caller-supplied repo_path, program and prompt (#3012 review). The scope is
//     this small because every route worth giving a sandbox is parameterised by a
//     host path or a session id, which route-level allowlisting cannot constrain
//     to the caller's own; that waits on binding the credential to its session;
//   - IN MEMORY ONLY, so a daemon restart invalidates every outstanding sandbox
//     credential. A long-running sandbox loses callback until it is
//     re-provisioned, which is the right side to fail on: a restart is exactly
//     when a credential minted by the previous process should stop working.
//
// It is deliberately NOT written to disk beside the operator token. There is no
// `af sandbox-token show`, and nothing renders one: the only copy outside this
// registry is the one injected into the sandbox that owns it.

// sandboxTokenRegistry maps live sandbox credentials to the session that owns
// them. Zero value is usable.
type sandboxTokenRegistry struct {
	mu sync.RWMutex
	// bySecret is the authorization lookup: presented credential -> session id.
	bySecret map[string]string
	// bySession is the revocation index, so ending a session drops its
	// credential without scanning. A session holds at most one at a time; a
	// re-provision replaces it, which implicitly revokes the old one.
	bySession map[string]string
}

// mint issues a fresh credential for sessionID, replacing and thereby revoking
// any credential that session already held.
func (r *sandboxTokenRegistry) mint(sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("cannot mint a sandbox callback token without a session id")
	}
	secret, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("cannot mint a sandbox callback token: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bySecret == nil {
		r.bySecret, r.bySession = map[string]string{}, map[string]string{}
	}
	if previous, ok := r.bySession[sessionID]; ok {
		delete(r.bySecret, previous)
	}
	r.bySecret[secret] = sessionID
	r.bySession[sessionID] = secret
	return secret, nil
}

// sessionFor returns the session a presented credential belongs to.
//
// The lookup is by map key rather than a constant-time compare against every
// entry. That is not a timing regression over the operator token: a map hit
// leaks only whether a secret exists, and the secret is 256 bits of CSPRNG
// output, so an attacker cannot walk the space with timing. Comparing linearly
// in constant time would leak the registry SIZE through latency instead.
func (r *sandboxTokenRegistry) sessionFor(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	sessionID, ok := r.bySecret[secret]
	return sessionID, ok
}

// revoke drops the credential a session holds. Idempotent: a session that never
// had one, or whose credential was already replaced, is a no-op success, because
// every teardown path calls this and none of them should fail for it.
func (r *sandboxTokenRegistry) revoke(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	secret, ok := r.bySession[sessionID]
	if !ok {
		return
	}
	delete(r.bySecret, secret)
	delete(r.bySession, sessionID)
}

// revokeAll drops every outstanding sandbox credential and reports how many it
// dropped, for the caller's log line.
//
// The control listener rebinding is the one event that calls this (#3012 review).
// A callback credential is inseparable from the endpoint it was minted for: the
// URL is baked into the sandbox's environment file at provision time and nothing
// rewrites it, so when bindWebLocked moves the listener and closes the old one,
// every already-running sandbox is left calling an address that no longer answers
// while its token stays valid. Leaving it valid buys nothing — the sandbox cannot
// reach the daemon to use it — and costs the ability to say what happened.
//
// This is the same rule the registry already documents for a daemon restart:
// credentials do not survive the listener they were issued against. Revoking makes
// the capability's disappearance explicit and logged, instead of a silent
// connection failure inside a sandbox nobody is watching. It is a MITIGATION, not
// the fix — the fix is a stable advertised callback endpoint that a rebind does not
// move, which is recorded on #2999.
func (r *sandboxTokenRegistry) revokeAll() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.bySession)
	r.bySecret, r.bySession = nil, nil
	return n
}

// requireTokenFixHint is the one-line fix named by the refusal below. Kept beside
// the refusal so the message and the key cannot drift.
const requireTokenFixHint = "af config set require_token true"

// errSandboxCallbackNeedsRequireToken refuses to hand a sandbox a credential for
// a listener that does not ask for one (#2999).
//
// This is the load-bearing refusal, so the reasoning is here rather than at the
// call site. require_token defaults to FALSE, and #2168 Phase 0 reversed #2090's
// refusal to bind a tokenless network listener — af now binds it and warns once.
// So the moment an operator points listen_addr somewhere a sandbox can reach, the
// whole control plane answers every caller with no credential at all.
//
// Injecting a token into that would be worse than doing nothing: it manufactures
// the appearance of a boundary that nothing enforces, and the next reader — human
// or agent — would reasonably believe the sandbox is limited to its scope when it
// is limited by nothing.
//
// Silently skipping the injection would be worse still. The sandbox would reach
// an UNAUTHENTICATED daemon and simply not know it, so the failure would surface
// as a mysterious capability rather than a refusal. Failing closed, naming the
// single config key that fixes it, is the only honest option.
func errSandboxCallbackNeedsRequireToken() error {
	return fmt.Errorf(
		"refusing to give this sandbox a callback credential: require_token is false, so the daemon's "+
			"listener accepts requests with no token and a scoped credential would enforce nothing. "+
			"Enable it first: %s",
		requireTokenFixHint,
	)
}

// mintSandboxCallback returns the credential and daemon URL to inject into a
// sandbox, or refuses.
//
// Order matters twice over. The posture is checked BEFORE a secret is generated,
// so a refused provision never leaves an unusable credential in the registry. And
// every address decision reads the listener that is ACTUALLY ACCEPTING rather than
// cfg.ListenAddr (#3012 review): the two diverge exactly when a live rebind failed,
// because ApplyConfig stores the new address while bindWebLocked deliberately
// leaves the old listener serving rather than making the daemon unreachable
// through the very API an operator would use to fix it. Reading config there would
// hand every sandbox a URL for an address nothing is bound to — a callback that
// silently never connects — and would judge the loopback posture against a
// listener that does not exist. #1856 hit the same divergence for the preview
// origin; activeWebConfigAddr is the same answer for this one.
func (m *Manager) mintSandboxCallback(cfg *config.Config, sessionID string) (url, token string, err error) {
	if cfg == nil {
		return "", "", errSandboxCallbackNeedsRequireToken()
	}
	// The generation is sampled BEFORE the address is read, not just before the mint
	// (#3012 review). Sampling it after left the address read itself outside the
	// window: a rebind landing in between would close the old listener, complete its
	// revokeAll, and then be recorded as the "starting" generation — so the final
	// comparison matched and the create finished with a live token paired to a
	// closed URL. Everything the credential depends on must be read inside the
	// window, and the address is the first of those reads.
	gen := m.webBindGeneration()
	active := m.activeWebConfigAddr(cfg.ListenAddr)
	if strings.TrimSpace(active) == "" && strings.TrimSpace(cfg.ListenAddr) != "" {
		return "", "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr is %q but no control-plane listener is accepting on it, so a callback would have nothing to reach. Check the daemon log for the bind failure and fix listen_addr", cfg.ListenAddr)
	}
	// Dialability first, posture second. Both refuse before any secret exists, but
	// the ORDER decides which message an operator reads, and a loopback listen_addr
	// used to answer "enable require_loopback_token" — advice that would have led
	// them to a well-enforced credential their sandbox still could not use.
	url, err = sandboxCallbackURL(active)
	if err != nil {
		return "", "", err
	}
	if err := sandboxCallbackPostureOK(cfg); err != nil {
		return "", "", err
	}
	// Snapshot the listener generation BEFORE minting and revalidate after, because
	// nothing holds webListeners.mu across the mint (#3012 review). A listen_addr
	// change racing this create would otherwise complete its rebind — including the
	// revokeAll sweep — in the window between reading the address and registering
	// the token, leaving a credential that survived the sweep while carrying the
	// URL of the listener the sweep just closed.
	//
	// Revalidate-and-refuse rather than lock, deliberately. Holding the listener
	// lock across a mint inverts the order bindWebLocked already uses (listener lock
	// -> revokeAll -> registry lock) and is a deadlock waiting to happen. Refusing
	// costs a create that the operator can retry against the new listener; the retry
	// is honest, and the alternative is a session that looks provisioned and has a
	// callback that never connects.
	token, err = m.sandboxTokens.mint(sessionID)
	if err != nil {
		return "", "", err
	}
	if m.webBindGeneration() != gen {
		m.sandboxTokens.revoke(sessionID)
		return "", "", fmt.Errorf("refusing to give this sandbox a callback credential: the control listener rebound while this session was being provisioned, so the callback address %q is already closed. Retry the create", url)
	}
	return url, token, nil
}

// webBindGeneration counts control-listener bindings, so a caller can tell whether
// the listener moved underneath it. 0 when there is no listener owner (a bare
// Manager, as tests construct), where nothing can rebind.
func (m *Manager) webBindGeneration() uint64 {
	if m.webListeners == nil {
		return 0
	}
	return m.webListeners.bindGeneration()
}

// activeWebConfigAddr is the config address behind the control-plane listener that
// is actually serving. It falls back to requested — the caller's own config value —
// only when there is no listener owner at all (a bare Manager, as tests construct),
// where the two cannot disagree because nothing has bound.
func (m *Manager) activeWebConfigAddr(requested string) string {
	if m.webListeners == nil {
		return requested
	}
	return m.webListeners.webConfigAddress()
}

// sandboxCallbackURL renders listen_addr as a URL a sandbox can dial, or refuses.
//
// It never rewrites an address into something routable: guessing an
// externally-reachable one would invent a fact about the operator's network, and a
// wrong guess points the agent at whatever answers there — far worse to debug than
// a named refusal. So every address that is not dialable FROM A SANDBOX is refused,
// and the three ways an address can fail that test are one rule, not three special
// cases: the address must name the daemon when resolved on the sandbox's side of
// the network, and must name a fixed port.
func sandboxCallbackURL(listenAddr string) (string, error) {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr is empty, so the daemon serves no HTTP listener for it to call back to. Set listen_addr to an address the sandbox can reach")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q is not host:port, so no callback URL can be derived from it", addr)
	}
	// A BIND address is not automatically a DIALABLE one, and the difference is not
	// cosmetic: a wildcard or empty host means "every interface HERE", which from
	// inside a sandbox names the SANDBOX, so the agent would quietly call itself.
	// Port 0 means "whatever the kernel picked", which this config value cannot know.
	//
	// Refused rather than guessed. Rewriting a wildcard into some interface address
	// would invent a fact about the operator's network, and a wrong guess points the
	// agent at whatever answers there — far worse to debug than a named refusal.
	// Tested by PARSING, not by spelling (#3012 review). An unspecified address has
	// many valid textual forms — "0.0.0.0", "::", the expanded "0:0:0:0:0:0:0:0",
	// the v4-mapped "::ffff:0.0.0.0" — and net.Listen binds every one of them the
	// same way, so a check against two literals recognised two of them and passed
	// the rest through. net.IP.IsUnspecified answers the question the literals were
	// approximating. An empty host is the same case with nothing to parse.
	if host == "" {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q binds every interface, which names the SANDBOX rather than the daemon when dialled from inside one. Set listen_addr to an address the sandbox can reach", addr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q binds every interface, which names the SANDBOX rather than the daemon when dialled from inside one. Set listen_addr to an address the sandbox can reach", addr)
	}
	// Loopback is the same failure as the wildcard above, and refusing it is the
	// correction to an earlier version of this function that let it through on the
	// reasoning that a sandbox "will simply fail to connect, which is a diagnosable
	// outcome" (#3012 review). It is not diagnosable — it is silently wrong.
	// 127.0.0.1 resolves INSIDE the sandbox, so the agent reaches its own loopback
	// and finds whatever is listening there, which is not this daemon.
	//
	// There is no tunnel to make it work, and that is a property of the design
	// rather than a gap: the ssh runtime opens a LOCAL forward, daemon → the remote
	// agent-server, so the daemon can drive the sandbox. Nothing forwards the other
	// way, and a callback is by definition the other way.
	//
	// Refusing here also removes the trap where the posture check told an operator
	// to set require_loopback_token — a key that would have bought them a
	// well-enforced credential for an address their sandbox can never dial.
	if config.IsLoopbackListenAddr(addr) {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q is loopback, so a sandbox dialling it reaches ITSELF rather than this daemon — and nothing forwards the other way (the ssh runtime tunnels daemon→sandbox only). Set listen_addr to an address the sandbox can reach", addr)
	}
	// The port is RESOLVED rather than string-matched, for the same reason as the
	// host above: "", "0", "00" and "000" are four spellings of port zero, they all
	// resolve to 0, and net.Listen treats every one as "let the kernel pick" — so
	// each literal I matched was one spelling out of an open-ended set (#3012
	// review). LookupPort is the same resolution net.Listen performs, so an address
	// it rejects could not have been bound either.
	resolved, perr := net.LookupPort("tcp", port)
	if perr != nil {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q has no resolvable port, so no callback URL can be derived from it", addr)
	}
	if resolved == 0 {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q asks the kernel to choose a port, so no fixed callback URL exists. Set an explicit port", addr)
	}
	// The RESOLVED port goes into the URL, so a service name ("…:http") becomes a
	// number the sandbox's HTTP client can dial rather than being passed through.
	return "http://" + net.JoinHostPort(host, strconv.Itoa(resolved)), nil
}

// sandboxCallbackPostureOK reports whether a credential handed out now would
// actually be CHECKED when it comes back (#3012 review).
//
// require_token alone does not establish that, and believing it did was the
// original bug in this refusal. authGate.authRequired exempts loopback peers
// BEFORE it looks at any token, so on a loopback listen_addr with
// require_loopback_token at its default false a same-host or host-network
// sandbox reaches the whole control plane with no credential — and therefore no
// scope at all. Minting there hands out something ceremonial and calls it a
// boundary.
//
// The predicate is "will this request be authenticated", not "is a config key
// set", which is why it mirrors the gate's own terms rather than restating them.
//
// It no longer carries a require_loopback_token branch. That check existed because
// authGate exempts loopback peers before examining any token, so a loopback
// listener would have enforced no scope — but sandboxCallbackURL now refuses every
// loopback address outright, as undialable from a sandbox, so this is only ever
// reached for an address the exemption cannot apply to. One rule, stated once,
// beats the same rule half-stated in two places (#3012 review).
func sandboxCallbackPostureOK(cfg *config.Config) error {
	if !cfg.RequireToken {
		return errSandboxCallbackNeedsRequireToken()
	}
	return nil
}
