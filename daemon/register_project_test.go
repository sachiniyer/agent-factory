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
