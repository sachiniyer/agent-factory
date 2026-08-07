package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credential subscribes to the RUNTIME's lifetime (#3068): provisioning a
// sandbox mints, reaping one revokes, and both happen in one place each.

type fakeCreds struct {
	mints    int
	revokes  int
	mintErr  error
	invalid  error
	lastCred SandboxCredential
}

func (f *fakeCreds) Mint() (SandboxCredential, error) {
	f.mints++
	if f.mintErr != nil {
		return SandboxCredential{}, f.mintErr
	}
	f.lastCred = SandboxCredential{
		URL:        "http://10.0.0.5:8443",
		Token:      "secret",
		Revalidate: func() error { return f.invalid },
	}
	return f.lastCred, nil
}

func (f *fakeCreds) Revoke() { f.revokes++ }

func TestMintForProvision_OnlyForKindsThatInjectACallback(t *testing.T) {
	creds := &fakeCreds{}

	// A local kind must not mint: minting unconditionally would make every local
	// create inherit a sandbox-only refusal.
	var spec ProvisionSpec
	cred, err := mintForProvision(&spec, BackendLocal, creds)
	require.NoError(t, err)
	assert.False(t, cred.Granted())
	assert.Equal(t, 0, creds.mints)
	assert.Empty(t, spec.CallbackToken)

	// An injecting kind mints and populates the spec.
	cred, err = mintForProvision(&spec, BackendSSH, creds)
	require.NoError(t, err)
	assert.True(t, cred.Granted())
	assert.Equal(t, 1, creds.mints)
	assert.Equal(t, "http://10.0.0.5:8443", spec.CallbackURL)
	assert.Equal(t, "secret", spec.CallbackToken)

	// No minter at all (a local session, a test, the in-sandbox agent-server) is a
	// no-op rather than an error.
	var bare ProvisionSpec
	cred, err = mintForProvision(&bare, BackendSSH, nil)
	require.NoError(t, err)
	assert.False(t, cred.Granted())
	assert.Empty(t, bare.CallbackToken)
}

// A refusal must FAIL the provision, not quietly produce a sandbox that cannot
// call back — the mint refuses exactly when a credential could not be enforced or
// could not be dialled.
func TestMintForProvision_PropagatesARefusal(t *testing.T) {
	refusal := errors.New("refusing to give this sandbox a callback credential: require_token is false")
	creds := &fakeCreds{mintErr: refusal}
	var spec ProvisionSpec
	_, err := mintForProvision(&spec, BackendSSH, creds)
	require.ErrorIs(t, err, refusal)
	assert.Empty(t, spec.CallbackToken, "a refused mint must leave the spec without a credential")
}

func TestRevalidateAfterProvision_RunsOnlyWhenSomethingWasGranted(t *testing.T) {
	assert.NoError(t, revalidateAfterProvision(SandboxCredential{}),
		"nothing was granted, so there is nothing to invalidate")

	invalidated := errors.New("invalidated")
	assert.ErrorIs(t, revalidateAfterProvision(SandboxCredential{
		Token: "secret", Revalidate: func() error { return invalidated },
	}), invalidated)

	assert.NoError(t, revalidateAfterProvision(SandboxCredential{
		Token: "secret", Revalidate: func() error { return nil },
	}))
}

// THE lifetime rule: a conclusively reaped runtime takes its credential with it,
// in ONE place, so kill/archive/restore/recover do not each have to remember.
//
// This is the half that stops the worse failure mode — a token minted for one
// runtime surviving into its replacement.
func TestResetRemoteRuntime_RevokesTheCredential(t *testing.T) {
	creds := &fakeCreds{}
	i := &Instance{Title: "worker"}
	i.SetSandboxCredentials(creds)

	i.resetRemoteRuntime()
	assert.Equal(t, 1, creds.revokes, "reaping a runtime must revoke the credential minted for it")

	// Idempotent: every teardown path reaches this and none should fail for it.
	i.resetRemoteRuntime()
	assert.Equal(t, 2, creds.revokes)

	// And an instance with no minter must not panic — that is every local session.
	bare := &Instance{Title: "local"}
	assert.NotPanics(t, func() { bare.resetRemoteRuntime() })
}

// A KNOWN teardown failure still means the runtime is gone, so the credential
// goes with it — while the runtime WIRING is deliberately left for the retry
// path (#3065 review).
//
// remoteAgentServer.Kill tears the in-sandbox workspace down over REST and then
// reaps the sandbox, and returns the REST error even when the reap succeeded. The
// success path's revocation is therefore not reachable on that branch, and
// leaving the credential live means a copied token keeps authenticating until
// some later recovery happens to replace it.
func TestRevokeSandboxCredential_IsSeparableFromResettingTheRuntime(t *testing.T) {
	creds := &fakeCreds{}
	i := &Instance{Title: "worker"}
	i.SetSandboxCredentials(creds)

	// Revoking alone must not disturb the wiring the retry path still needs.
	i.runtimeTeardown = func() error { return nil }
	i.revokeSandboxCredential()
	assert.Equal(t, 1, creds.revokes)
	assert.NotNil(t, i.runtimeTeardown, "settling the credential must not clear the wiring the abort-to-Lost retry owns")

	// And the full reset still revokes, so the two agree on the common path.
	i.resetRemoteRuntime()
	assert.Equal(t, 2, creds.revokes)
	assert.Nil(t, i.runtimeTeardown)

	bare := &Instance{Title: "local"}
	assert.NotPanics(t, func() { bare.revokeSandboxCredential() })
}
