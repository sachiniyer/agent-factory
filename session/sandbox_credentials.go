package session

import "github.com/sachiniyer/agent-factory/log"

// The sandbox callback credential's binding to a RUNTIME's lifetime (#3068).
//
// The credential (#2999) is minted for a specific provisioned sandbox and bound
// to the session that owns it (#3056). The lifecycle question this file answers
// is the one four review rounds kept re-opening: a session's runtime is not
// permanent. Archive restore and Lost-session recovery REAP the old sandbox and
// provision a replacement, and a credential minted for the reaped one is wrong in
// both directions —
//
//   - carried forward, it is a token minted for one runtime being presented from
//     another, which is worse than having none;
//   - dropped, the recovered agent silently loses the ability to call af, with
//     nothing on any surface explaining why.
//
// So the credential SUBSCRIBES to the runtime's lifetime rather than being
// refreshed by whoever remembers. Two rules, each in exactly one place:
//
//	MINT on provisioning a sandbox — mintForProvision, called by the create path
//	and the replacement path through the same function.
//	REVOKE on the runtime being conclusively reaped — Instance.resetRemoteRuntime,
//	which is reached only when a teardown SUCCEEDED or failed knowably, never when
//	an unknown outcome retains the old wiring for a retry.
//
// That second rule is why ordering is correct by construction rather than by
// care: a replacement provisions after the reap, the reap revokes, and the mint
// that follows cannot strand a credential on a runtime that is still alive. An
// earlier attempt minted before the reap and permanently severed callback from
// exactly that retained runtime (#3065 review).
//
// Three separate sites — listener rebind, listener teardown, unexpected listener
// death — each had to be taught the invalidation rule independently before it was
// moved onto the state it describes. This is the same correction applied to the
// runtime half.

// SandboxCredential is one minted callback credential: what to inject, plus the
// check that says it is still valid.
type SandboxCredential struct {
	// URL and Token are injected into the sandbox as AF_DAEMON_URL and
	// AF_DAEMON_TOKEN. Empty when no credential was granted.
	URL, Token string
	// Revalidate reports whether the grant still holds. Provisioning is the long
	// window — an ssh clone and a binary copy — and a listener move or an auth
	// posture change inside it revokes the credential and closes its URL while the
	// sandbox has already had the stale pair written in. Every path that provisions
	// must call this AFTER Provision returns, not only the create path: doing it in
	// one and not the other is how the two drifted the first time.
	//
	// nil when there is nothing to revalidate.
	Revalidate func() error
}

// Granted reports whether a credential was actually issued.
func (c SandboxCredential) Granted() bool { return c.Token != "" }

// SandboxCredentials mints and revokes one session's callback credential. The
// daemon implements it; package session holds only this interface, so the runtime
// lifecycle can drive the credential without session depending on the daemon's
// registry.
//
// Nil on an Instance means no daemon is backing it — a local session, or an
// instance built by a test or by the agent-server inside a sandbox — and every
// call site treats nil as "no credential", never as an error.
type SandboxCredentials interface {
	// Mint issues a fresh credential for this session, REPLACING and thereby
	// revoking any credential the session already held. Replacing is the point on
	// the reprovision path: the sandbox being replaced may still hold the old
	// secret, and it must stop working.
	Mint() (SandboxCredential, error)
	// Revoke drops the session's credential. Idempotent — every teardown path
	// calls it and none should fail for it.
	Revoke()
}

// mintForProvision fills spec's callback fields for a kind that provisions a
// sandbox, and returns the credential so the caller can revalidate it after
// provisioning.
//
// THE one place both the create path and the replacement path mint, which is the
// whole point: the previous shape had create minting inside the backend factory
// and reprovision building its own ProvisionSpec by hand, so the replacement path
// simply had no credential — and when that was fixed by adding a second mint call
// site, that site immediately drifted on live-config, ordering, and revalidation.
//
// A kind that does not inject a callback, or a session with no daemon behind it,
// returns a zero credential and leaves spec untouched. A mint that REFUSES is
// propagated: the refusals exist for postures where a credential could not be
// enforced or could not be dialled, and provisioning a sandbox that will silently
// fail to call back is the outcome this feature exists to prevent.
func mintForProvision(spec *ProvisionSpec, kind BackendKind, creds SandboxCredentials) (SandboxCredential, error) {
	if !kind.InjectsSandboxCallback() {
		// This kind never had a callback; nothing is lost and nothing to say.
		return SandboxCredential{}, nil
	}
	if creds == nil {
		// A kind that WOULD take a credential, with nothing able to mint one. Said
		// out loud rather than returned as a silent zero (#3068): losing callback
		// quietly is one of the two failure modes this file exists to prevent, and
		// the shape that produces it — an instance the daemon never attached a
		// minter to — is exactly the bug that shipped and was invisible.
		//
		// Not an error, because it is also the ordinary state for an instance with
		// no daemon behind it (a test, the agent-server inside a sandbox). A line in
		// the log is the difference between "this build has no callback here" and
		// "this session silently lost one".
		log.WarningLog.Printf("provisioning a %s sandbox with no callback credential: nothing is available to mint one, so its agent will not be able to call af", kind)
		return SandboxCredential{}, nil
	}
	cred, err := creds.Mint()
	if err != nil {
		return SandboxCredential{}, err
	}
	spec.CallbackURL, spec.CallbackToken = cred.URL, cred.Token
	return cred, nil
}

// revalidateAfterProvision re-checks a credential once the sandbox it was minted
// for exists. Returns nil when there is nothing to check.
func revalidateAfterProvision(cred SandboxCredential) error {
	if !cred.Granted() || cred.Revalidate == nil {
		return nil
	}
	return cred.Revalidate()
}

// SetSandboxCredentials attaches the daemon-backed minter to an instance the
// daemon materialized from disk.
//
// Exported for exactly that: FromInstanceData rebuilds an inert instance and
// cannot know about the daemon, so without this every session loaded after a
// restart would provision its replacement with no credential and no error —
// which is what the first version of this shipped (#3065 review).
//
// Set-once in practice, but written under i.mu because reprovisionRemote reads it
// under the read lock while a restore may be materializing instances.
func (i *Instance) SetSandboxCredentials(c SandboxCredentials) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sandboxCreds = c
}

// SandboxCredentialsAttached reports whether this instance can mint a callback
// credential for a sandbox provisioned on its behalf.
//
// Exported narrowly so the daemon can ASSERT its own wiring: the failure this
// guards is an instance materialized from disk that nobody attached a minter to,
// which produces no error and no symptom until a restore quietly comes back
// without a callback (#3065 review). A predicate the daemon's tests can read is
// the difference between that being caught and being shipped.
func (i *Instance) SandboxCredentialsAttached() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.sandboxCreds != nil
}
