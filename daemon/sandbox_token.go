package daemon

import (
	"fmt"
	"net"
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
	active := m.activeWebConfigAddr(cfg.ListenAddr)
	if strings.TrimSpace(active) == "" && strings.TrimSpace(cfg.ListenAddr) != "" {
		return "", "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr is %q but no control-plane listener is accepting on it, so a callback would have nothing to reach. Check the daemon log for the bind failure and fix listen_addr", cfg.ListenAddr)
	}
	if err := sandboxCallbackPostureOK(cfg, active); err != nil {
		return "", "", err
	}
	url, err = sandboxCallbackURL(active)
	if err != nil {
		return "", "", err
	}
	token, err = m.sandboxTokens.mint(sessionID)
	if err != nil {
		return "", "", err
	}
	return url, token, nil
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

// sandboxCallbackURL renders listen_addr as a URL a sandbox can dial, or "" when
// there is nothing to dial.
//
// A loopback listen_addr is returned unchanged rather than rewritten to something
// routable. Guessing an externally-reachable address here would be inventing a
// fact about the operator's network — the sandbox will simply fail to connect,
// which is a diagnosable outcome, whereas a wrong guess silently points the agent
// at whatever answers that address on ITS side of the network.
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
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q binds every interface, which names the SANDBOX rather than the daemon when dialled from inside one. Set listen_addr to an address the sandbox can reach", addr)
	}
	if port == "0" {
		return "", fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr %q asks the kernel to choose a port, so no fixed callback URL exists. Set an explicit port", addr)
	}
	return "http://" + net.JoinHostPort(host, port), nil
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
// activeAddr is the address of the listener actually accepting, NOT cfg.ListenAddr:
// the loopback exemption is decided by the listener a callback really reaches, so
// judging it against an address that failed to bind would clear the posture for a
// listener that does not exist (#3012 review).
func sandboxCallbackPostureOK(cfg *config.Config, activeAddr string) error {
	if !cfg.RequireToken {
		return errSandboxCallbackNeedsRequireToken()
	}
	if config.IsLoopbackListenAddr(activeAddr) && !cfg.RequireLoopbackToken {
		return fmt.Errorf("refusing to give this sandbox a callback credential: listen_addr is loopback and require_loopback_token is false, so the gate exempts same-host callers before it checks any token and the credential's scope would enforce nothing. Enable it: %s", requireLoopbackTokenFixHint)
	}
	return nil
}

// requireLoopbackTokenFixHint is the one-line fix for the loopback posture.
const requireLoopbackTokenFixHint = "af config set require_loopback_token true"
