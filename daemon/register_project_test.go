package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForEvent waits for the next event of type want on ch, failing if none
// arrives. EventProjectsChanged carries a nil payload, so unlike
// drainNextSessionEvent this asserts only on the type.
func waitForEvent(t *testing.T, ch <-chan agentproto.Event, want agentproto.EventType) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event published within the deadline", want)
		}
	}
}

// assertNoEvent fails if any event of type unwanted arrives within a short
// window. Used to prove a rejected registration publishes nothing.
func assertNoEvent(t *testing.T, ch <-chan agentproto.Event, unwanted agentproto.EventType) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Type == unwanted {
				t.Fatalf("a %s event was published when none was expected", unwanted)
			}
		case <-deadline:
			return
		}
	}
}

// TestControlServer_RegisterProject_PersistsIdempotentPublishes is #2456: the
// RPC records a git checkout as a durable project, returns its identity, makes
// it visible to ListProjects, and publishes projects.changed so a client
// refreshes. Registering the same checkout again — and a subdirectory of it — is
// an idempotent success that returns the SAME project id.
func TestControlServer_RegisterProject_PersistsIdempotentPublishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()

	var resp RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: repoPath}, &resp))
	require.True(t, resp.OK)
	require.NotEmpty(t, resp.Project.ID, "a registered project must carry a durable id")
	assert.Equal(t, filepath.Clean(repoPath), resp.Project.Root, "the id resolves to the checkout root")

	waitForEvent(t, ch, agentproto.EventProjectsChanged)

	projects, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "the registered project must be listed")
	assert.Equal(t, resp.Project.ID, projects[0].ID)

	// Idempotent: registering the same checkout again returns the same identity.
	var again RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: repoPath}, &again))
	assert.Equal(t, resp.Project.ID, again.Project.ID, "re-registering a known checkout is a no-op success")

	// A subdirectory resolves to the same canonical root, not a second project.
	sub := filepath.Join(repoPath, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	var fromSub RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: sub}, &fromSub))
	assert.Equal(t, resp.Project.ID, fromSub.Project.ID, "a subdirectory registers the checkout's root, not itself")

	projects, err = config.ListProjects()
	require.NoError(t, err)
	assert.Len(t, projects, 1, "idempotent re-registration must not create duplicates")
}

// TestControlServer_RegisterProject_RejectsNonGitPath: a path that is not inside
// a git checkout is refused with an error, nothing is persisted, and no
// projects.changed event fires.
func TestControlServer_RegisterProject_RejectsNonGitPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	plain := filepath.Join(testguard.CanonicalTempDir(t), "not-a-repo")
	require.NoError(t, os.MkdirAll(plain, 0o755))

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()

	var resp RegisterProjectResponse
	err = cs.RegisterProject(RegisterProjectRequest{Path: plain}, &resp)
	require.Error(t, err, "a non-git path must be refused")
	assert.False(t, resp.OK)

	assertNoEvent(t, ch, agentproto.EventProjectsChanged)

	projects, err := config.ListProjects()
	require.NoError(t, err)
	assert.Empty(t, projects, "a rejected registration must persist nothing")
}

// TestControlServer_ListProjects_ReadsRegistry is #2456's read half — the source
// the TUI/web switcher unions with their derived project lists. The RPC returns the
// durable registry, empty when nothing is registered and reflecting a registration
// once one lands.
func TestControlServer_ListProjects_ReadsRegistry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	// An empty registry reads as an empty list (not an error).
	var empty ListProjectsResponse
	require.NoError(t, cs.ListProjects(ListProjectsRequest{}, &empty))
	assert.Empty(t, empty.Projects, "an empty registry lists nothing")

	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: repoPath}, &reg))

	var resp ListProjectsResponse
	require.NoError(t, cs.ListProjects(ListProjectsRequest{}, &resp))
	require.Len(t, resp.Projects, 1, "the registered project must be listed")
	assert.Equal(t, reg.Project.ID, resp.Projects[0].ID)
	assert.Equal(t, filepath.Clean(repoPath), resp.Projects[0].Root)
}

// TestControlServer_ListProjects_NotGatedWhenWarming pins the flip side of
// RegisterProject's admission gate: ListProjects is a pure READ, allowed during
// probation (controlMethodPolicies), so a warming manager still answers it — where
// RegisterProject would return the daemon-starting error. A read that blocked while
// the daemon warmed would leave a client's switcher briefly, silently empty.
func TestControlServer_ListProjects_NotGatedWhenWarming(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	shell, err := newManagerShell(config.DefaultConfig())
	require.NoError(t, err)
	require.False(t, shell.Ready(), "precondition: the manager shell must not report ready")

	notReady := &controlServer{manager: shell}
	var resp ListProjectsResponse
	err = notReady.ListProjects(ListProjectsRequest{}, &resp)
	require.NoError(t, err, "ListProjects is a read; it must answer even while the manager warms")
	assert.Empty(t, resp.Projects)
}

// TestControlServer_DeleteProject_RemovesRegistryRecordAndPublishes is #2456's
// symmetric counterpart to RegisterProject: deleting a registered project with NO
// live sessions removes its durable registry record AND still publishes
// projects.changed (the archived/killed counts are zero), so a client's switcher
// union drops it. Without BOTH halves a registered project could never leave the
// list — config.ListProjects would keep re-adding it, and with no event nothing
// would prompt a refetch.
func TestControlServer_DeleteProject_RemovesRegistryRecordAndPublishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var reg RegisterProjectResponse
	require.NoError(t, cs.RegisterProject(RegisterProjectRequest{Path: repoPath}, &reg))
	projects, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "precondition: the project is registered")

	// Subscribe AFTER the register so we assert on the DELETE's event, not the add's.
	_, ch := manager.events.subscribe()

	var del DeleteProjectResponse
	require.NoError(t, cs.DeleteProject(DeleteProjectRequest{RepoPath: repoPath}, &del))
	require.True(t, del.OK)
	assert.Equal(t, 0, del.ArchivedCount, "a sessionless project archives nothing")
	assert.Equal(t, 0, del.KilledCount)

	waitForEvent(t, ch, agentproto.EventProjectsChanged)

	projects, err = config.ListProjects()
	require.NoError(t, err)
	assert.Empty(t, projects, "the deleted project's registry record is gone, so the switcher drops it")
}

// TestControlServer_RegisterProject_GatedWhenWarming: like every state mutation,
// RegisterProject is refused while the manager is still warming up, with the
// daemon-starting error clients retry on.
func TestControlServer_RegisterProject_GatedWhenWarming(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	shell, err := newManagerShell(config.DefaultConfig())
	require.NoError(t, err)
	require.False(t, shell.Ready(), "precondition: the manager shell must not report ready")

	notReady := &controlServer{manager: shell}
	var resp RegisterProjectResponse
	err = notReady.RegisterProject(RegisterProjectRequest{Path: t.TempDir()}, &resp)
	assert.True(t, IsDaemonStartingErr(err), "RegisterProject on a warming manager: want daemon-starting error, got: %v", err)
}
