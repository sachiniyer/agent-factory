package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

func stubRemote(t *testing.T) {
	t.Helper()
	prev := isRemoteTarget
	isRemoteTarget = func() bool { return true }
	t.Cleanup(func() { isRemoteTarget = prev })
	// A seeded local registry and tasks.json must never be read.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

func TestRemoteProjectListComesFromTheTargetsRegistry(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{appConfig: &config.Config{RootAgents: map[string]config.RootAgentConfig{"/local/only": {}}}}
	m.remote.apply(&remoteReads{projects: []config.Project{
		{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa", CheckoutID: "c1", PathExists: true},
		{ID: "prj_fedcba9876543210fedcba9876543210", Root: "/srv/box/beta", RepoID: "bbbbbbbbbbbb"},
	}})
	data := []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: "/srv/box/alpha"}}}
	projects, degraded := m.buildProjectListFrom(data)
	require.False(t, degraded)
	require.Len(t, projects, 2, "a local root_agents key is not a project of the target")
	require.Equal(t, "alpha", projects[0].Name)
	require.Equal(t, "aaaaaaaaaaaa", projects[0].RepoID)
	require.Equal(t, 1, projects[0].SessionCount)
	require.Equal(t, "prj_0123456789abcdef0123456789abcdef", projects[0].RegistryID)
	require.Equal(t, "beta", projects[1].Name)
}

func TestRemoteProjectListKeepsStaleRowsAndReportsDegradedWhenTheTargetFails(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{}
	m.remote.apply(&remoteReads{projects: []config.Project{{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa"}}})
	m.remote.apply(&remoteReads{projectsErr: errors.New("connection refused")})
	projects, degraded := m.buildProjectListFrom(nil)
	require.True(t, degraded, "a failed refresh is reported, not shown as an empty registry")
	require.Len(t, projects, 1, "the last good rows stay on screen")

	cold := &home{}
	_, degraded = cold.buildProjectListFrom(nil)
	require.True(t, degraded, "nothing read yet is not a complete registry either")
}

// A target that accepts the connection and never answers must cost the poll's
// goroutine a bounded wait, must leave no goroutine behind, and must cost the
// event loop nothing.
func TestRemoteHungTargetNeverBlocksTheEventLoop(t *testing.T) {
	stubRemote(t)
	pp, pt, pto := remoteListProjects, remoteListTasks, remoteReadTimeout
	t.Cleanup(func() { remoteListProjects, remoteListTasks, remoteReadTimeout = pp, pt, pto })
	// A hung target: only the caller's context ends the read.
	remoteListProjects = func(ctx context.Context) ([]config.Project, error) { <-ctx.Done(); return nil, ctx.Err() }
	remoteListTasks = func(ctx context.Context) ([]task.Task, error) { <-ctx.Done(); return nil, ctx.Err() }
	remoteReadTimeout = 100 * time.Millisecond

	// The off-loop read gives up at the deadline and says why. It returns only
	// after both workers have, so nothing is left running past this line.
	start := time.Now()
	reads := fetchRemoteReads(context.Background())
	require.Less(t, time.Since(start), 2*time.Second)
	require.ErrorContains(t, reads.projectsErr, "did not answer")
	require.ErrorContains(t, reads.tasksErr, "did not answer")

	// The model's lifetime context also ends the read, well before the deadline.
	remoteReadTimeout = time.Minute
	owner, cancelOwner := context.WithCancel(context.Background())
	finished := make(chan *remoteReads, 1)
	go func() { finished <- fetchRemoteReads(owner) }()
	cancelOwner()
	select {
	case r := <-finished:
		require.Error(t, r.projectsErr)
	case <-time.After(5 * time.Second):
		t.Fatal("the read outlived the context of the model that owns it")
	}

	// Every loop-side consumer reads the cache and never dials, so none can wait
	// on the target (the stubs above would block until their context ends).
	m := &home{repoID: "aaaaaaaaaaaa", repoRoot: "/srv/box/alpha"}
	m.remote.apply(reads)
	_, degraded := m.buildProjectListFrom(nil)
	require.True(t, degraded)
	_, err := m.activeTasks(m.repoRoot, m.repoID)
	require.NoError(t, err)
	_, err = m.repoContextForSelection("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err)
}

// Every selection entry point resolves through the target's row, not local git.
func TestRemoteSelectionNeverProbesLocalGit(t *testing.T) {
	stubRemote(t)
	m := &home{sidebarProjects: []overlay.Project{{Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa"}}}
	rc, err := m.repoContextForSelection("/srv/box/alpha", "") // sidebar Enter / mouse: root only
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaaaaaa", rc.ID)
	rc, err = m.repoContextForSelection("/srv/box/alpha", "aaaaaaaaaaaa") // picker
	require.NoError(t, err)
	require.Equal(t, "/srv/box/alpha", rc.Root)
	_, err = m.repoContextForSelection("/srv/box/unknown", "")
	require.ErrorContains(t, err, "not in the daemon's project list")
}

func TestRemoteTasksAreScopedToTheProjectFromTheCache(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{}
	_, err := m.activeTasks("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err, "nothing read yet and no error: an empty list, not the local tasks.json")
	m.remote.apply(&remoteReads{tasks: []task.Task{
		{ID: "t1", RepoID: "aaaaaaaaaaaa"},
		{ID: "t2", RepoID: "bbbbbbbbbbbb"},
		{ID: "t3", ProjectPath: "/srv/box/alpha"},
		{ID: "t4", ProjectPath: "/srv/box/alpha/sub"},
		{ID: "t5"},
	}})
	got, err := m.activeTasks("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t3"}, remoteTaskIDs(got))
	none, err := m.activeTasks("", "")
	require.NoError(t, err)
	require.Empty(t, none, "registry mode has no active task scope")
}

func TestRemoteRepoContextIsBuiltFromTheRowNotALocalProbe(t *testing.T) {
	rc := remoteRepoContext("/srv/box/alpha", "aaaaaaaaaaaa")
	require.Equal(t, "/srv/box/alpha", rc.WorkspacePath())
	require.Equal(t, "aaaaaaaaaaaa", rc.ID)
}

func TestRemoteHooksSaveIsRefusedNotWrittenLocally(t *testing.T) {
	stubRemote(t)
	require.ErrorContains(t, errRemoteHooksSave(), "edit .agent-factory/config.toml there")
}

func failOnDial(t *testing.T) {
	t.Helper()
	pp, pt := remoteListProjects, remoteListTasks
	remoteListProjects = func(context.Context) ([]config.Project, error) {
		t.Error("event loop dialed the target for projects")
		return nil, nil
	}
	remoteListTasks = func(context.Context) ([]task.Task, error) {
		t.Error("event loop dialed the target for tasks")
		return nil, nil
	}
	t.Cleanup(func() { remoteListProjects, remoteListTasks = pp, pt })
}

func remoteTaskIDs(ts []task.Task) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids
}
