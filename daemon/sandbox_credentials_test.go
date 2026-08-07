package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The half the first attempt missed: an instance materialized from DISK has no
// credential minter unless the daemon attaches one (#3065 review, #3068).
//
// session.FromInstanceData cannot populate it — package session knows nothing
// about the daemon — so without this every session loaded after a daemon restart
// provisions its replacement sandbox with no callback at all, silently. That is
// the ordinary restore, not an edge case, which is why the first fix looked like
// it worked and did nothing.
func TestRefresh_AttachesCredentialsToDiskLoadedInstances(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedDiskInstance(t, repoID, "restored", repoPath)

	manager.mu.Lock()
	err := manager.refreshLocked()
	manager.mu.Unlock()
	require.NoError(t, err)

	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "restored")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the seeded row must materialize, or this test asserts nothing")

	assert.True(t, inst.SandboxCredentialsAttached(),
		"a disk-loaded instance reached the manager with no credential minter: a restore would provision its "+
			"replacement sandbox with no callback and report success")
}

// The minter reads LIVE config, never a snapshot captured when it was built.
//
// A stored closure holding create-time config was the previous shape: if the
// operator later disabled require_token, ApplyConfig revoked every outstanding
// credential and a re-mint through that closure still saw RequireToken=true — so
// it succeeded against a listener that by then authenticated nobody, and the
// invalidation predated the new mint's fence, so the fence could not catch it.
func TestSandboxCredentials_MintReadsTheLivePosture(t *testing.T) {
	m := applyConfigTestManager(t)
	creds := newSandboxCredentials(m, "sess-a")

	// require_token defaults to false, so a mint must REFUSE: a scoped credential
	// against a listener that demands none enforces nothing.
	_, err := creds.Mint()
	require.Error(t, err, "a credential against a tokenless listener enforces nothing")
	assert.Contains(t, err.Error(), requireTokenFixHint)

	// Flip the posture LIVE. The same minter, already built, must see it — that is
	// the whole point: the previous shape captured config when it was constructed.
	_, err = config.SetGlobalConfigValue("require_token", "true")
	require.NoError(t, err)
	_, err = config.SetGlobalConfigValue("listen_addr", "10.0.0.5:8443")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)

	cred, err := creds.Mint()
	require.NoError(t, err, "the minter must read the posture live, not the one captured when it was built")
	assert.Equal(t, "http://10.0.0.5:8443", cred.URL)
	assert.NotEmpty(t, cred.Token)

	// And it revokes through the same object, which is what the runtime lifecycle
	// calls when it reaps a sandbox.
	owner, ok := m.sandboxTokens.sessionFor(cred.Token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)
	creds.Revoke()
	_, ok = m.sandboxTokens.sessionFor(cred.Token)
	assert.False(t, ok, "revoking through the credentials object must reach the registry")
}
