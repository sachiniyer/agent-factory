package daemon

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
)

// The sandbox callback credential (#2999).
//
// A provisioned sandbox can be driven BY the daemon but could not call back INTO
// it, so a remote agent could not do the things a local one does by running `af`.
// Giving it the operator's own token would have been the easy answer and the
// wrong one: that token grants the whole control plane including DeliverPrompt,
// which runs instructions through every agent on the machine — so one compromised
// sandbox would own the host.
//
// What a sandbox gets instead is a token that is:
//
//   - PER SESSION, so it can be revoked with that session and names who used it;
//   - SCOPED, admitting only the routes a remote agent legitimately needs, with
//     every route denied unless it opts in (see HTTPRoute.sandboxAllowed);
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
// Order matters: the posture is checked BEFORE a secret is generated, so a
// refused provision never leaves an unusable credential in the registry.
func (m *Manager) mintSandboxCallback(cfg *config.Config, sessionID string) (url, token string, err error) {
	if cfg == nil || !cfg.RequireToken {
		return "", "", errSandboxCallbackNeedsRequireToken()
	}
	url = sandboxCallbackURL(cfg.ListenAddr)
	if url == "" {
		return "", "", fmt.Errorf(
			"refusing to give this sandbox a callback credential: listen_addr is empty, so the daemon serves " +
				"no HTTP listener for it to call back to. Set listen_addr to an address the sandbox can reach")
	}
	token, err = m.sandboxTokens.mint(sessionID)
	if err != nil {
		return "", "", err
	}
	return url, token, nil
}

// sandboxCallbackURL renders listen_addr as a URL a sandbox can dial, or "" when
// there is nothing to dial.
//
// A loopback listen_addr is returned unchanged rather than rewritten to something
// routable. Guessing an externally-reachable address here would be inventing a
// fact about the operator's network — the sandbox will simply fail to connect,
// which is a diagnosable outcome, whereas a wrong guess silently points the agent
// at whatever answers that address on ITS side of the network.
func sandboxCallbackURL(listenAddr string) string {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return ""
	}
	return "http://" + addr
}
