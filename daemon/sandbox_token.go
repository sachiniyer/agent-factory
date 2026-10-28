package daemon

import (
	"fmt"
	"strings"
	"sync"
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
	// layout returns the daemon host's layout strings, withheld from every response
	// for a request that authenticated with a credential from this registry. Set
	// once at construction, before any request.
	layout func() []string

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

// errSandboxEndpointDown is the refusal while the dedicated sandbox endpoint is
// not listening. It is the only precondition a callback now has (#5571 W3.4):
// the credential's carrier is the endpoint, not the client listener, so
// network.listen_addr and network.require_token no longer decide anything here.
// The create degrades through the W3.1 path rather than failing.
func errSandboxEndpointDown() error {
	return fmt.Errorf("refusing to give this sandbox a callback credential: the daemon's sandbox callback endpoint is not running, so a callback would have nothing to reach. Check the daemon log for its bind failure")
}

// mintSandboxCallbackFenced issues a sandbox callback credential with the fence
// sampled by the CALLER, for a caller that reads an input of its own first.
//
// The fence must be sampled before every input the credential depends on is
// read, so an invalidation landing between the read and the sample cannot be
// recorded as the starting value (#3012, #3065 reviews). The only input left is
// the endpoint's liveness; the caller opens the window.
func (m *Manager) mintSandboxCallbackFenced(fence uint64, sessionID string) (sandboxGrant, error) {
	socket, err := m.sandboxEndpointOrRefusal()
	if err != nil {
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
		return sandboxGrant{}, errSandboxCallbackInvalidated(socket)
	}
	return sandboxGrant{Token: token, Socket: socket, fence: fence}, nil
}

// sandboxEndpointOrRefusal returns the socket the running endpoint listens on,
// or the refusal the create path will degrade with (#5571 W3.1). Shared by the
// mint and the picker's callback_unavailable_reason, so choose-time and
// create-time give one answer.
func (m *Manager) sandboxEndpointOrRefusal() (string, error) {
	if path := m.sandboxEndpointSocket.Load(); path != nil && *path != "" {
		return *path, nil
	}
	return "", errSandboxEndpointDown()
}

// setSandboxEndpointSocket records (or, with "", clears) the live endpoint.
func (m *Manager) setSandboxEndpointSocket(path string) {
	m.sandboxEndpointSocket.Store(&path)
}

// callbackUnavailableReason answers "would a sandbox created right now get a
// callback?" with the refusal text when it would not — "" when it would.
func (m *Manager) callbackUnavailableReason() string {
	if _, err := m.sandboxEndpointOrRefusal(); err != nil {
		return err.Error()
	}
	return ""
}

// sandboxGrant is an issued callback credential plus the fence it was issued
// under, so the CALLER can revalidate later — after provisioning, which is the
// slow part and therefore the widest window (#3065 review). Minting revalidates
// its own window; only the create knows when provisioning finished.
type sandboxGrant struct {
	Token string
	// Socket is the daemon-side endpoint socket the -R forward terminates on. The
	// sandbox-side address is the provisioner's to choose (a socket inside the
	// session's own directory), so the grant carries no URL.
	Socket string
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

// errSandboxCallbackInvalidated refuses a credential that was voided while it was
// being issued, or while the sandbox it was issued for was provisioned.
func errSandboxCallbackInvalidated(socket string) error {
	return fmt.Errorf("refusing to give this sandbox a callback credential: outstanding callback credentials were revoked while this session was being provisioned, so the callback to %q may already be dead. Retry the create", socket)
}
