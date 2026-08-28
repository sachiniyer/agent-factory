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
	// invalidations counts events that voided EVERY outstanding credential at once
	// — a listener rebind, a teardown, an unexpected listener death, or an auth
	// posture relaxed to tokenless. It is the fence a mint checks across its whole
	// window (#3065 review).
	//
	// It lives here rather than on the listener, and that IS the fix: the counter it
	// replaces was the listener's binding generation, so it advanced for socket
	// changes and stayed put for an auth-only change like network.require_token going false
	// — and a mint in flight across that change survived the sweep meant to catch
	// it. Every invalidation routes through revokeAll, so counting revokeAll counts
	// invalidations by construction, whatever causes the next one.
	invalidations uint64
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
	// Advanced on every CALL, not only when something was dropped. A sweep that
	// finds the registry empty still means an invalidating event happened, and the
	// mint racing it has not registered its token yet — exactly the case a count
	// gated on n > 0 would miss.
	r.invalidations++
	return n
}

// invalidationCount is the fence a mint compares across its window. A change
// between two reads means every credential outstanding at any point in between was
// voided, including one registered mid-window.
func (r *sandboxTokenRegistry) invalidationCount() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.invalidations
}

// requireTokenFixHint is the one-line fix named by the refusal below. Kept beside
// the refusal so the message and the key cannot drift.
const requireTokenFixHint = "af config set network.require_token true"

// errSandboxCallbackNeedsRequireToken refuses to hand a sandbox a credential for
// a listener that does not ask for one (#2999).
//
// This is the load-bearing refusal, so the reasoning is here rather than at the
// call site. network.require_token defaults to FALSE, and #2168 Phase 0 reversed #2090's
// refusal to bind a tokenless network listener — af now binds it and warns once.
// So the moment an operator points network.listen_addr somewhere a sandbox can reach, the
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
		"refusing to give this sandbox a callback credential: network.require_token is false, so the daemon's "+
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
func (m *Manager) mintSandboxCallback(cfg *config.Config, sessionID string) (sandboxGrant, error) {
	return m.mintSandboxCallbackFenced(m.sandboxTokens.invalidationCount(), cfg, sessionID)
}

// mintSandboxCallbackFenced is mintSandboxCallback with the fence sampled by the
// CALLER, for a caller that reads an input of its own first.
//
// The daemon's live minter reads m.Config() before calling in, and an argument is
// evaluated before the function it is passed to runs — so sampling the fence in
// here would leave that config read outside the window, and an invalidation
// landing between the two would complete its sweep before the window opened
// (#3065 review). The fence would then compare equal on both sides and the mint
// would proceed on config that was already stale when it was read.
//
// This is the third time this exact boundary has moved: first the fence was
// sampled after the address read, then after the config read moved out to the
// caller. The rule that survives all three is the one stated below — every input
// the credential depends on must be read INSIDE the window — and the only way to
// keep it true when a caller acquires an input is to let the caller open the
// window.
func (m *Manager) mintSandboxCallbackFenced(fence uint64, cfg *config.Config, sessionID string) (sandboxGrant, error) {
	if cfg == nil {
		return sandboxGrant{}, errSandboxCallbackNeedsRequireToken()
	}
	// The fence is sampled BEFORE the address is read, not merely before the mint
	// (#3012 review): an invalidation landing between the read and the sample would
	// complete its sweep and then be recorded as the STARTING value, so the final
	// comparison matched and the create finished with a live token paired to a dead
	// endpoint. Everything the credential depends on is read inside the window, and
	// the address is the first of those reads.
	//
	// It counts INVALIDATIONS, not listener rebinds (#3065 review). The listener
	// generation could not see an auth-only change: network.require_token going false
	// revokes every credential without touching a socket, so the generation stayed
	// put and a mint in flight across it survived the sweep — provisioning a sandbox
	// against a daemon that had just stopped authenticating anyone.
	active := m.activeWebConfigAddr(cfg.ListenAddr)
	if strings.TrimSpace(active) == "" && strings.TrimSpace(cfg.ListenAddr) != "" {
		return sandboxGrant{}, fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr is %q but no control-plane listener is accepting on it, so a callback would have nothing to reach. Check the daemon log for the bind failure and fix network.listen_addr", cfg.ListenAddr)
	}
	// Dialability first, posture second. Both refuse before any secret exists, but
	// the ORDER decides which message an operator reads, and a loopback network.listen_addr
	// used to answer "enable network.require_loopback_token" — advice that would have led
	// them to a well-enforced credential their sandbox still could not use.
	url, err := sandboxCallbackURL(active)
	if err != nil {
		return sandboxGrant{}, err
	}
	if err := sandboxCallbackPostureOK(cfg); err != nil {
		return sandboxGrant{}, err
	}
	// Revalidate-and-refuse rather than lock, deliberately. Holding the listener
	// lock across a mint inverts the order bindWebLocked already uses (listener lock
	// -> revokeAll -> registry lock) and is a deadlock waiting to happen. Refusing
	// costs a create the operator can retry; the alternative is a session that looks
	// provisioned with a callback that never connects.
	token, err := m.sandboxTokens.mint(sessionID)
	if err != nil {
		return sandboxGrant{}, err
	}
	if m.sandboxTokens.invalidationCount() != fence {
		m.sandboxTokens.revoke(sessionID)
		return sandboxGrant{}, errSandboxCallbackInvalidated(url)
	}
	return sandboxGrant{URL: url, Token: token, fence: fence}, nil
}

// sandboxGrant is an issued callback credential plus the fence it was issued
// under, so the CALLER can revalidate later — after provisioning, which is the
// slow part and therefore the widest window (#3065 review). Minting revalidates
// its own window; only the create knows when provisioning finished.
type sandboxGrant struct {
	URL   string
	Token string
	// fence is the registry's invalidation count at the moment this grant's inputs
	// were first read. Unexported: it is meaningless outside a comparison against
	// invalidationCount(), and nothing should serialize or log it.
	fence uint64
}

// stillValid reports whether nothing has invalidated outstanding credentials since
// this grant was issued.
func (g sandboxGrant) stillValid(r *sandboxTokenRegistry) bool {
	return r.invalidationCount() == g.fence
}

// errSandboxCallbackInvalidated refuses a credential whose premises changed while
// it was being issued, or while the sandbox it was issued for was provisioned.
//
// One message for both windows on purpose: from the operator's side they are the
// same event — something voided every outstanding credential while this create was
// in flight — and the same action answers both.
func errSandboxCallbackInvalidated(url string) error {
	return fmt.Errorf("refusing to give this sandbox a callback credential: the daemon's callback posture changed while this session was being provisioned — the control listener moved or closed, or network.require_token was disabled — so the callback address %q may already be dead. Retry the create", url)
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

// sandboxCallbackURL renders network.listen_addr as a URL a sandbox can dial, or refuses.
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
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr is empty, so the daemon serves no HTTP listener for it to call back to. Set network.listen_addr to an address the sandbox can reach")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q is not host:port, so no callback URL can be derived from it", addr)
	}
	// A BIND address is not automatically a DIALABLE one, and the difference is not
	// cosmetic: a wildcard or empty host means "every interface HERE", which from
	// inside a sandbox names the SANDBOX, so the agent would quietly call itself.
	// Port 0 means "whatever the kernel picked", which this config value cannot know.
	//
	// Refused rather than guessed. Rewriting a wildcard into some interface address
	// would invent a fact about the operator's network, and a wrong guess points the
	// agent at whatever answers there — far worse to debug than a named refusal.
	// ONE predicate for the host, because the previous three rounds of this review
	// each found another spelling the last one missed: a wildcard, then the expanded
	// and v4-mapped unspecified forms, then loopback, then loopback ALIASES like
	// ip6-localhost, then zone-scoped addresses. Enumerating spellings loses.
	//
	// The host must be a LITERAL IP. That is not a convenience — it is the only form
	// whose meaning does not depend on who resolves it, and the resolver that matters
	// here is the SANDBOX'S, not this daemon's. "ip6-localhost" was the visible case
	// of a general fact: af cannot know what a name denotes on the other side of the
	// network, so it must not put one in a callback URL. Requiring a literal also
	// rejects zone-scoped addresses (net.ParseIP refuses "fe80::1234%eth0") — which
	// is right twice over, since a zone names an interface on the DAEMON'S host and
	// would additionally emit a raw "%" that Go's URL parser reads as a bad escape.
	//
	// With the host guaranteed to be an IP, the two undialable classes are semantic
	// questions with exact answers, rather than string comparisons:
	//   - IsUnspecified — binds every interface, so it names the SANDBOX.
	//   - IsLoopback — names the sandbox ITSELF, and nothing forwards a callback back
	//     (the ssh runtime tunnels daemon→sandbox only).
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q does not name a literal IP address, and a hostname is resolved by the SANDBOX rather than by this daemon — af cannot know what it points at over there. Set network.listen_addr to a literal IP the sandbox can reach", addr)
	}
	if ip.IsUnspecified() {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q binds every interface, which names the SANDBOX rather than the daemon when dialled from inside one. Set network.listen_addr to an address the sandbox can reach", addr)
	}
	if ip.IsLoopback() {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q is loopback, so a sandbox dialling it reaches ITSELF rather than this daemon — and nothing forwards the other way (the ssh runtime tunnels daemon→sandbox only). Set network.listen_addr to an address the sandbox can reach", addr)
	}
	// The port is RESOLVED rather than string-matched, for the same reason as the
	// host above: "", "0", "00" and "000" are four spellings of port zero, they all
	// resolve to 0, and net.Listen treats every one as "let the kernel pick" — so
	// each literal I matched was one spelling out of an open-ended set (#3012
	// review). LookupPort is the same resolution net.Listen performs, so an address
	// it rejects could not have been bound either.
	resolved, perr := net.LookupPort("tcp", port)
	if perr != nil {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q has no resolvable port, so no callback URL can be derived from it", addr)
	}
	if resolved == 0 {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: network.listen_addr %q asks the kernel to choose a port, so no fixed callback URL exists. Set an explicit port", addr)
	}
	// The RESOLVED port goes into the URL, so a service name ("…:http") becomes a
	// number the sandbox's HTTP client can dial rather than being passed through.
	return "http://" + net.JoinHostPort(host, strconv.Itoa(resolved)), nil
}

// sandboxCallbackPostureOK reports whether a credential handed out now would
// actually be CHECKED when it comes back (#3012 review).
//
// network.require_token alone does not establish that, and believing it did was the
// original bug in this refusal. authGate.authRequired exempts loopback peers
// BEFORE it looks at any token, so on a loopback network.listen_addr with
// network.require_loopback_token at its default false a same-host or host-network
// sandbox reaches the whole control plane with no credential — and therefore no
// scope at all. Minting there hands out something ceremonial and calls it a
// boundary.
//
// The predicate is "will this request be authenticated", not "is a config key
// set", which is why it mirrors the gate's own terms rather than restating them.
//
// It no longer carries a network.require_loopback_token branch. That check existed because
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
