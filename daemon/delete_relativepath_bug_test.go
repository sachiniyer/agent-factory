package daemon

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestControlServer_DeleteProject_RefusesRelativePath mirrors
// TestControlServer_RebindProject_RefusesRelativePath (added by commit
// 6616c129): a relative RepoPath must be refused at the RPC boundary, because
// the daemon resolves what it is sent against ITS own cwd — an unrelated
// checkout for an ad-hoc daemon, / under systemd — and would silently delete
// whatever project it lands on. RegisterProject and RebindProject both enforce
// this via ResolveDaemonHostPath; DeleteProject is the only destructive
// projects-family RPC that lacked the same guard, so this test closes the
// deviation from the documented, tested boundary (#4821, 6616c129).
func TestControlServer_DeleteProject_RefusesRelativePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	original := setupControlRepo(t)
	unrelated := filepath.Join(testguard.CanonicalTempDir(t), "unrelated")
	cloneRepoTemplate(t, unrelated)
	t.Chdir(unrelated)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: original}, &reg))
	_, ch := manager.events.subscribe()

	for _, rel := range []string{".", "sub/../."} {
		var resp DeleteProjectResponse
		err := cs.DeleteProject(&DeleteProjectRequest{RepoPath: rel}, &resp)
		require.Error(t, err, "relative RepoPath %q must be refused, not resolved against the daemon's cwd", rel)
		assert.Contains(t, err.Error(), "must be absolute", "relative RepoPath %q", rel)
		assert.False(t, resp.OK, "a refused delete must not report success")
		assert.False(t, resp.Deregistered, "a refused delete must not report a deregistration")
	}

	projects, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "a refused relative delete must leave the registry untouched")
	assert.Equal(t, reg.Project.Root, projects[0].Root)
	assertNoEvent(t, ch, agentproto.EventProjectsChanged)
}

// TestControlServer_DeleteProject_UsesTheNormalizedPath mirrors
// TestControlServer_RebindProject_UsesTheNormalizedPath (#4789 round 7): the
// boundary check trims the path, so the mutation must receive that same trimmed
// value. A whitespace-prefixed absolute path — sent while the daemon's cwd is an
// unrelated checkout — must delete the project at the absolute path it names,
// never resolve as relative under the cwd. Checking a trimmed copy while
// passing the raw one would re-open the hole ResolveDaemonHostPath closes.
func TestControlServer_DeleteProject_UsesTheNormalizedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	target := setupControlRepo(t)
	unrelated := filepath.Join(testguard.CanonicalTempDir(t), "unrelated")
	cloneRepoTemplate(t, unrelated)
	t.Chdir(unrelated)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: target}, &reg))
	_, ch := manager.events.subscribe()

	// A whitespace-padded absolute path is trimmed at the boundary and used to
	// delete the project it names, not resolved as relative under the cwd.
	var resp DeleteProjectResponse
	require.NoError(t, cs.DeleteProject(&DeleteProjectRequest{RepoPath: "  " + target + "\t"}, &resp))
	require.True(t, resp.OK)
	assert.True(t, resp.Deregistered, "the trimmed absolute path's project was deregistered")

	waitForEvent(t, ch, agentproto.EventProjectsChanged)

	projects, err := config.ListProjects()
	require.NoError(t, err)
	assert.Empty(t, projects, "the project named by the trimmed absolute path was deregistered")
}
