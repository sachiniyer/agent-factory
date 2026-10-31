package daemon

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
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
//   - DERIVED, NOT STORED (#5571 W3.6): the registry is memory-only, but the
//     secret is HMAC(runtime-identity key, session id, ref), so a row persists
//     only the non-secret ref. A restarted daemon re-derives and re-registers the
//     very secret a still-running sandbox holds (adopt, driven by the reattach),
//     and nothing is re-delivered. Without the key it cannot be re-derived, so a
//     lost key fails closed rather than minting a stranger. Neither a listener
//     rebind nor a require_token change revokes it: the credential's carrier is
//     the dedicated sandbox endpoint (W3.4), which neither touches.
//
// It is deliberately NOT written to disk beside the operator token. There is no
// `af sandbox-token show`, and nothing renders one: the only copy outside this
// registry is the one injected into the sandbox that owns it; the row stores
// only the ref it derives from.

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
	secret, _, err := r.mintDerived(sessionID)
	return secret, err
}

// mintDerived is mint plus the opaque ref the credential derives from (#5571
// W3.6). The secret is not random: it is HMAC(runtime-identity key, session id,
// ref), so the row can persist only the ref and a restarted daemon re-derives
// and re-registers the very secret the sandbox already holds (adopt).
func (r *sandboxTokenRegistry) mintDerived(sessionID string) (secret, ref string, err error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", "", fmt.Errorf("cannot mint a sandbox callback token without a session id")
	}
	if ref, err = runtimetoken.NewTokenRef(); err != nil {
		return "", "", fmt.Errorf("cannot mint a sandbox callback token: %w", err)
	}
	if secret, err = r.adopt(sessionID, ref, true); err != nil {
		return "", "", err
	}
	return secret, ref, nil
}

// adopt registers the credential derived from ref for sessionID, replacing any
// the session already held, and returns it. ensureKey creates the key when
// absent (a fresh mint); a restart passes false, because a key that is gone
// means the credential the sandbox holds can no longer be re-derived and
// inventing a new one would only mint a stranger.
func (r *sandboxTokenRegistry) adopt(sessionID, ref string, ensureKey bool) (string, error) {
	var key []byte
	var err error
	if ensureKey {
		key, err = runtimetoken.EnsureKey()
	} else {
		key, err = runtimetoken.LoadKey()
	}
	if err != nil {
		return "", fmt.Errorf("cannot derive a sandbox callback token: %w", err)
	}
	secret, err := runtimetoken.DeriveCallbackToken(key, sessionID, ref)
	if err != nil {
		return "", err
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

// stillGrants reports whether secret is still sessionID's credential: false once
// the session's teardown, archive or replacement revoked or replaced it. A route
// that does slow work after the gate admitted a request re-checks the very
// credential it was admitted with (push_credential.go).
func (r *sandboxTokenRegistry) stillGrants(sessionID, secret string) bool {
	if sessionID == "" || secret == "" {
		return false
	}
	owner, ok := r.sessionFor(secret)
	return ok && owner == sessionID
}

// grantFor is the credential sessionID holds right now, "" for none.
func (r *sandboxTokenRegistry) grantFor(sessionID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bySession[sessionID]
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
// dropped, advancing the invalidation fence a mint compares across its window.
//
// Since #5571 W3.6 nothing in the daemon calls this: it used to run on a
// listener rebind, teardown, unexpected death and require_token going false,
// because the callback rode that listener. It now rides the dedicated sandbox
// endpoint, which none of those events touch, so revoking on them only cut
// working callbacks. The primitive and its fence stay as the one way to void the
// whole registry at once (an endpoint rebind, a key rotation) so such an event
// cannot be reinvented per call site; removing the fence with the session-side
// revalidation hook it feeds is a follow-up.
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
	token, ref, err := m.sandboxTokens.mintDerived(sessionID)
	if err != nil {
		return sandboxGrant{}, err
	}
	if m.sandboxTokens.invalidationCount() != fence {
		m.sandboxTokens.revoke(sessionID)
		return sandboxGrant{}, errSandboxCallbackInvalidated(socket)
	}
	return sandboxGrant{Token: token, Ref: ref, Socket: socket, fence: fence}, nil
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
	// Ref is the opaque reference Token derives from; the row persists it so a
	// restarted daemon can re-register the same credential (W3.6).
	Ref string
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
