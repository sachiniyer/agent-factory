package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
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
	prev := remoteListProjects
	remoteListProjects = func() ([]config.Project, error) {
		return []config.Project{
			{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa", CheckoutID: "c1", PathExists: true},
			{ID: "prj_fedcba9876543210fedcba9876543210", Root: "/srv/box/beta", RepoID: "bbbbbbbbbbbb"},
		}, nil
	}
	t.Cleanup(func() { remoteListProjects = prev })

	m := &home{appConfig: &config.Config{RootAgents: map[string]config.RootAgentConfig{"/local/only": {}}}}
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

func TestRemoteProjectListDegradesWhenTheTargetIsUnreachable(t *testing.T) {
	stubRemote(t)
	prev := remoteListProjects
	remoteListProjects = func() ([]config.Project, error) { return nil, errors.New("connection refused") }
	t.Cleanup(func() { remoteListProjects = prev })

	m := &home{}
	_, degraded := m.buildProjectListFrom(nil)
	require.True(t, degraded, "a failed remote read is reported as incomplete, not as an empty registry")
}

func TestRemoteTasksComeFromTheTargetAndAreScopedToTheProject(t *testing.T) {
	stubRemote(t)
	prev := remoteListTasks
	remoteListTasks = func() ([]task.Task, error) {
		return []task.Task{
			{ID: "t1", RepoID: "aaaaaaaaaaaa"},
			{ID: "t2", RepoID: "bbbbbbbbbbbb"},
			{ID: "t3", ProjectPath: "/srv/box/alpha"},
			{ID: "t4", ProjectPath: "/srv/box/alpha/sub"},
			{ID: "t5"},
		}, nil
	}
	t.Cleanup(func() { remoteListTasks = prev })

	got, err := tasksForScope("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t3"}, remoteTaskIDs(got))

	none, err := tasksForScope("", "")
	require.NoError(t, err)
	require.Empty(t, none, "registry mode has no active task scope")

	remoteListTasks = func() ([]task.Task, error) { return nil, errors.New("boom") }
	_, err = tasksForScope("/srv/box/alpha", "aaaaaaaaaaaa")
	require.Error(t, err, "no fallback to the local tasks.json")
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

func remoteTaskIDs(ts []task.Task) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids
}
