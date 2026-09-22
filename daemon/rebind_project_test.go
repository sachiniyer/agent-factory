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

// TestControlServer_RebindProject_MovesIdentityAndPublishes: the RPC repoints a
// registered project's stable id at a replacement checkout — the moved,
// recloned, or renamed case every surface now routes through the daemon for —
// returns the rebound record, and publishes projects.changed so a client's
// switcher refetches rather than trusting the pre-rebind row.
func TestControlServer_RebindProject_MovesIdentityAndPublishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	oldPath := setupControlRepo(t)
	newPath := filepath.Join(testguard.CanonicalTempDir(t), "replacement")
	cloneRepoTemplate(t, newPath)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: oldPath}, &reg))

	// Subscribe AFTER the register so we assert on the REBIND's event.
	_, ch := manager.events.subscribe()

	var resp RebindProjectResponse
	require.NoError(t, cs.RebindProject(RebindProjectRequest{ID: reg.Project.ID, Path: newPath}, &resp))
	require.True(t, resp.OK)
	assert.Equal(t, reg.Project.ID, resp.Project.ID, "rebind keeps the stable project id")
	assert.Equal(t, filepath.Clean(newPath), resp.Project.Root, "the id now resolves to the replacement checkout")

	waitForEvent(t, ch, agentproto.EventProjectsChanged)

	projects, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "rebind moves the one record rather than adding a second")
	assert.Equal(t, filepath.Clean(newPath), projects[0].Root)
}

// TestControlServer_RebindProject_RejectsUnknownID: a project id with no
// registry record is refused, nothing changes, and no event fires.
func TestControlServer_RebindProject_RejectsUnknownID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	newPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()

	var resp RebindProjectResponse
	err = cs.RebindProject(RebindProjectRequest{ID: "prj_doesnotexist", Path: newPath}, &resp)
	require.Error(t, err, "an unregistered project id must be refused")
	assert.False(t, resp.OK)

	assertNoEvent(t, ch, agentproto.EventProjectsChanged)
}

// TestControlServer_RebindProject_RefusesOwnedPath: rebinding onto a checkout
// another project already owns is rejected — the registry must not end up with
// two ids resolving to one root.
func TestControlServer_RebindProject_RefusesOwnedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	first := setupControlRepo(t)
	second := filepath.Join(testguard.CanonicalTempDir(t), "second")
	cloneRepoTemplate(t, second)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var regA, regB RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: first}, &regA))
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: second}, &regB))

	var resp RebindProjectResponse
	err = cs.RebindProject(RebindProjectRequest{ID: regA.Project.ID, Path: second}, &resp)
	require.Error(t, err, "a path owned by another project must be refused")
	assert.False(t, resp.OK)
}

// TestControlServer_RebindProject_GatedWhenWarming: like every state mutation,
// RebindProject is refused while the manager is still warming up, with the
// daemon-starting error clients retry on.
func TestControlServer_RebindProject_GatedWhenWarming(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	shell, err := newManagerShell(config.DefaultConfig())
	require.NoError(t, err)
	require.False(t, shell.Ready(), "precondition: the manager shell must not report ready")

	notReady := &controlServer{manager: shell}
	var resp RebindProjectResponse
	err = notReady.RebindProject(RebindProjectRequest{ID: "prj_anything", Path: t.TempDir()}, &resp)
	assert.True(t, IsDaemonStartingErr(err), "RebindProject on a warming manager: want daemon-starting error, got: %v", err)
}
