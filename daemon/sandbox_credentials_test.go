package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
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
// Driven through fromInstanceDataForRefresh, the seam that exists so a test can
// "observe (or substitute) the call". Two earlier versions of this test tried to
// get a real row to materialize — seeding disk and refreshing, then persisting and
// restarting — and in both the row never loaded, so the test asserted nothing
// while looking like coverage. Substituting the materializer removes the fixture
// from the question entirely: what is under test is whether the manager attaches
// credentials to whatever it loads, not whether a particular row is loadable.
func TestRefresh_AttachesCredentialsToDiskLoadedInstances(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedDiskInstanceWithID(t, repoID, "sess-restored", "restored", repoPath)

	restore := fromInstanceDataForRefresh
	t.Cleanup(func() { fromInstanceDataForRefresh = restore })

	// Stands in for a disk-materialized instance: built with no SandboxCredentials,
	// exactly as FromInstanceData leaves one.
	bornWithout := false
	fromInstanceDataForRefresh = func(d session.InstanceData) (*session.Instance, error) {
		inst, err := session.NewInstance(session.InstanceOptions{ID: d.ID, Title: d.Title, Path: d.Path, Program: "claude"})
		if err != nil {
			return nil, err
		}
		// Anti-vacuous: if it arrived already attached, the assertion below would
		// pass without the manager having done anything.
		bornWithout = !inst.SandboxCredentialsAttached()
		return inst, nil
	}

	manager.mu.Lock()
	err := manager.refreshLocked()
	manager.mu.Unlock()
	require.NoError(t, err)
	require.True(t, bornWithout, "the substitute must produce an UNATTACHED instance, or this test proves nothing")

	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "restored")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the seeded row must materialize through the substitute")

	assert.True(t, inst.SandboxCredentialsAttached(),
		"a disk-loaded instance reached the manager with no credential minter: restoring it would provision a "+
			"replacement sandbox with no callback and report success")
}

// seedDiskInstanceWithID is seedDiskInstance with a stable id, which the
// credential attach keys on.
func seedDiskInstanceWithID(t *testing.T, repoID, id, title, repoPath string) {
	t.Helper()
	seeded, err := json.Marshal([]session.InstanceData{
		{ID: id, Title: title, Path: repoPath, Status: session.Running},
	})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))
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
