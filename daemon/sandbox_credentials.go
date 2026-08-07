package daemon

import "github.com/sachiniyer/agent-factory/session"

// The daemon's implementation of session.SandboxCredentials (#3068).
//
// It is what lets the session runtime drive the credential from the runtime's
// own lifetime — mint when a sandbox is provisioned, revoke when one is reaped —
// without package session knowing anything about the registry, the listener, or
// the auth posture.
//
// One instance of this per session, carrying only the session id. Everything
// else is read LIVE at call time, which is the correction to the first attempt:
// that one stored a closure capturing the config from create time, so a re-mint
// hours later still saw the posture the session was born under. If the operator
// had disabled require_token in between, ApplyConfig revoked every outstanding
// credential and the stale snapshot still said "require_token is true" — so the
// re-mint succeeded against a listener that by then authenticated nobody, and it
// happened BEFORE the new mint sampled its fence, so the fence could not catch it
// either. A fence protects a window; it cannot protect inputs that were already
// wrong when the window opened (#3065 review).
type sandboxCredentials struct {
	manager   *Manager
	sessionID string
}

// newSandboxCredentials binds a credential minter to one session.
func newSandboxCredentials(m *Manager, sessionID string) *sandboxCredentials {
	return &sandboxCredentials{manager: m, sessionID: sessionID}
}

// Mint issues a fresh credential, replacing (and so revoking) any the session
// already held.
//
// m.Config() is read HERE, per call, never captured. The refusals inside
// mintSandboxCallback then evaluate the posture that is live at the moment a
// sandbox is actually being provisioned, which is the only posture that matters.
func (c *sandboxCredentials) Mint() (session.SandboxCredential, error) {
	if c == nil || c.manager == nil {
		return session.SandboxCredential{}, nil
	}
	// Fence FIRST, then read config. An argument is evaluated before the call it is
	// passed to, so `mint(m.Config(), …)` would read the posture outside the window
	// and an invalidation landing in between would finish its sweep before the
	// window opened — the fence comparing equal on both sides while the config it
	// guards was already stale (#3065 review).
	fence := c.manager.sandboxTokens.invalidationCount()
	grant, err := c.manager.mintSandboxCallbackFenced(fence, c.manager.Config(), c.sessionID)
	if err != nil {
		return session.SandboxCredential{}, err
	}
	registry := &c.manager.sandboxTokens
	return session.SandboxCredential{
		URL:   grant.URL,
		Token: grant.Token,
		// Carries the fence with it, so whoever provisions can re-check it after —
		// the create path and the replacement path both do, through the same
		// helper. The first version handed back only url+token, and the fence it
		// had just introduced was dropped on the floor by the second call site.
		Revalidate: func() error {
			if grant.stillValid(registry) {
				return nil
			}
			return errSandboxCallbackInvalidated(grant.URL)
		},
	}, nil
}

// Revoke drops this session's credential. Idempotent, and a no-op for a session
// that never held one — every teardown path calls it and none should fail for it.
func (c *sandboxCredentials) Revoke() {
	if c == nil || c.manager == nil {
		return
	}
	c.manager.sandboxTokens.revoke(c.sessionID)
}

// attachSandboxCredentials gives an instance the daemon-backed minter, so a
// replacement sandbox provisioned for it can re-mint.
//
// Called for instances LOADED FROM DISK, which is the half the first attempt
// missed entirely: session.FromInstanceData reconstructs an inert instance and
// has no way to populate this, so every restore after a daemon restart — the
// ordinary restore — silently skipped minting while looking like it worked
// (#3065 review). A created instance gets it through InstanceOptions instead.
//
// Deliberately unconditional on backend kind. Whether a credential is issued is
// decided at provision time by BackendKind.InjectsSandboxCallback, from the kind
// the instance actually has THEN; deciding it here, from the kind it had when it
// was loaded, would be a second and staler copy of that predicate.
func attachSandboxCredentials(m *Manager, inst *session.Instance) *session.Instance {
	if m == nil || inst == nil || inst.ID == "" {
		return inst
	}
	inst.SetSandboxCredentials(newSandboxCredentials(m, inst.ID))
	return inst
}
