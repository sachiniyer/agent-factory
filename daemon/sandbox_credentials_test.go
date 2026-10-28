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

// The minter reads the endpoint's LIVE state, never a snapshot captured when it
// was built, and does not consult the client listener's address or auth posture
// (#5571 W3.4).
func TestSandboxCredentials_MintReadsTheLiveEndpoint(t *testing.T) {
	m := applyConfigTestManager(t)
	creds := newSandboxCredentials(m, "sess-a")

	// Default config: loopback listener, require_token false. Both used to refuse.
	_, err := creds.Mint()
	require.Error(t, err, "no endpoint is listening yet")

	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	cred, err := creds.Mint()
	require.NoError(t, err, "the minter must read the endpoint live, not the state captured when it was built")
	assert.Equal(t, "/tmp/af-home/sandbox-callback.sock", cred.Socket)
	assert.Regexp(t, `^http://127\.0\.0\.1:[0-9]+$`, cred.URL)
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
