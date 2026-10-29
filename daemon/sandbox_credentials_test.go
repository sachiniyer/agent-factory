package daemon

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
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
	assert.Empty(t, cred.URL, "the sandbox-side address is the provisioner's, never a TCP port")
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

// The callback credential survives a daemon restart (#5571 W3.6): the registry
// is memory-only, but the secret is derived from the persisted ref, so a fresh
// registry re-registers the very secret the sandbox already holds.
func TestSandboxCallbackCredentialSurvivesARegistryRestart(t *testing.T) {
	m := applyConfigTestManager(t)
	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	cred, err := newSandboxCredentials(m, "sess-a").Mint()
	require.NoError(t, err)
	require.NotEmpty(t, cred.Ref, "the grant must carry the ref the row persists")

	// A restarted daemon: an empty registry over the same AF home (same key).
	m.sandboxTokens.revokeAll()
	_, ok := m.sandboxTokens.sessionFor(cred.Token)
	require.False(t, ok, "anti-vacuous: nothing is registered before the restore")

	require.NoError(t, newSandboxCredentials(m, "sess-a").RestoreCallback(cred.Ref))
	owner, ok := m.sandboxTokens.sessionFor(cred.Token)
	require.True(t, ok, "the secret the sandbox already holds must be accepted again with no re-delivery")
	assert.Equal(t, "sess-a", owner)

	// The ref is bound to the session: restoring it for another session
	// registers a different secret, never the first session's.
	require.NoError(t, newSandboxCredentials(m, "sess-b").RestoreCallback(cred.Ref))
	owner, ok = m.sandboxTokens.sessionFor(cred.Token)
	require.True(t, ok)
	assert.Equal(t, "sess-a", owner, "session b's restore must not take over session a's credential")
}

// A restore never invents a key: with the key gone the credential the sandbox
// holds cannot be re-derived, and a fresh key would only derive a stranger.
func TestSandboxCallbackRestoreRefusesWithoutTheKey(t *testing.T) {
	m := applyConfigTestManager(t)
	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	cred, err := newSandboxCredentials(m, "sess-a").Mint()
	require.NoError(t, err)

	keyPath, err := runtimetoken.KeyPath()
	require.NoError(t, err)
	require.NoError(t, os.Remove(keyPath))
	require.Error(t, newSandboxCredentials(m, "sess-a").RestoreCallback(cred.Ref))
	_, statErr := os.Stat(keyPath)
	assert.True(t, os.IsNotExist(statErr), "a failed restore must not write a replacement key")
}

// A restore whose callback endpoint is not bound registers nothing and says why
// (#5897): sshd would accept the persisted -R to a socket nobody listens on, so
// reporting the callback restored would leave every call dead.
func TestSandboxCallbackRestoreRefusesWhileTheEndpointIsDown(t *testing.T) {
	m := applyConfigTestManager(t)
	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	cred, err := newSandboxCredentials(m, "sess-a").Mint()
	require.NoError(t, err)
	m.sandboxTokens.revokeAll()

	m.setSandboxEndpointSocket("") // the endpoint failed to bind on this start
	err = newSandboxCredentials(m, "sess-a").RestoreCallback(cred.Ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), errSandboxEndpointDown().Error())
	_, ok := m.sandboxTokens.sessionFor(cred.Token)
	assert.False(t, ok, "no credential may be registered for a callback path that is dead")

	m.setSandboxEndpointSocket("/tmp/af-home/sandbox-callback.sock")
	require.NoError(t, newSandboxCredentials(m, "sess-a").RestoreCallback(cred.Ref))
	_, ok = m.sandboxTokens.sessionFor(cred.Token)
	assert.True(t, ok, "control: with the endpoint bound the same restore registers")
}
