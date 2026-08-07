package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The half the first attempt missed: an instance materialized from DISK has no
// credential minter unless the daemon attaches one (#3065 review, #3068).
//
// session.FromInstanceData cannot populate it — package session knows nothing
// about the daemon — so without this every session loaded after a daemon restart
// provisions its replacement sandbox with no callback at all, silently. That is
// the ordinary restore, not an edge case, which is why the first fix looked like
// it worked and did nothing.
//
// Driven through a REAL restart — persist, then a second Manager that loads from
// disk — because that is the situation. Seeding a row and refreshing in place does
// not reproduce it: my first version of this test did that, and the row never
// materialized at all, so it asserted nothing.
func TestRestoreInstances_AttachesCredentialsToDiskLoadedInstances(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	data, err := createForTask(manager, repoPath, "task1", "restored", 1)
	require.NoError(t, err)

	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	require.NotNil(t, inst)
	require.True(t, inst.SandboxCredentialsAttached(), "a created instance gets its minter through InstanceOptions")
	manager.persistInstance(repo.ID, inst)

	// The restart. This manager has never seen the session; it builds it from disk.
	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())

	restarted.mu.Lock()
	loaded := restarted.instances[daemonInstanceKey(repo.ID, data.Title)]
	restarted.mu.Unlock()
	require.NotNil(t, loaded, "the persisted row must load, or this test asserts nothing")

	assert.True(t, loaded.SandboxCredentialsAttached(),
		"a disk-loaded instance reached the manager with no credential minter: restoring it would provision a "+
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

	// A dialable listen_addr first. DefaultConfig's is loopback, which is refused
	// for a reason of its own — a sandbox dialling it reaches itself — and that
	// refusal would mask the posture one this test is about. (My first version
	// asserted the require_token hint here and got the loopback message, because
	// dialability is checked first.)
	_, err := config.SetGlobalConfigValue("listen_addr", "10.0.0.5:8443")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)

	// require_token is still false, so a mint must REFUSE: a scoped credential
	// against a listener that demands none enforces nothing.
	_, err = creds.Mint()
	require.Error(t, err)
	assert.Contains(t, err.Error(), requireTokenFixHint)

	// Flip the posture LIVE. The same minter, already built, must see it — that is
	// the whole point: the previous shape captured its config at construction.
	_, err = config.SetGlobalConfigValue("require_token", "true")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)

	cred, err := creds.Mint()
	require.NoError(t, err, "the minter must read the posture live, not the one captured when it was built")
	assert.Equal(t, "http://10.0.0.5:8443", cred.URL)
	assert.NotEmpty(t, cred.Token)

	// And it revokes through the same object — what the runtime lifecycle calls
	// when it reaps a sandbox.
	owner, ok := m.sandboxTokens.sessionFor(cred.Token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner)
	creds.Revoke()
	_, ok = m.sandboxTokens.sessionFor(cred.Token)
	assert.False(t, ok, "revoking through the credentials object must reach the registry")
}
