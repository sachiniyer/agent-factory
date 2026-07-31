package daemon

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskMutations_UnresolvedProjectIdentityFailsClosed(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	missingProject := filepath.Join(repoPath, "not-created")
	server := archiveTaskControlServer(manager)

	err := server.AddTask(AddTaskRequest{Task: archiveTargetTask(
		"unknown1", "Unknown Project", missingProject, "worker", true,
	)}, &AddTaskResponse{})
	require.Error(t, err, "an enabled targeted task cannot be validated without repository identity")
	assert.Contains(t, err.Error(), "determine project identity")
	_, getErr := task.GetTask("unknown1")
	require.Error(t, getErr, "an indeterminate target relationship must not commit")
}

func TestTaskMutations_MissingReservedTargetRequiresMaterialization(t *testing.T) {
	t.Run("configured root", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoPath := setupControlRepo(t)
		repo, err := config.RepoFromPath(repoPath)
		require.NoError(t, err)
		cfg := config.DefaultConfig()
		cfg.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
		require.NoError(t, config.SaveConfig(cfg))
		manager, err := NewManager(cfg)
		require.NoError(t, err)
		require.True(t, manager.repoRootAgentWillMaterialize(repo.ID))

		err = archiveTaskControlServer(manager).AddTask(AddTaskRequest{Task: archiveTargetTask(
			"rootwait", "Configured Root", repoPath, session.RootSessionTitle, true,
		)}, &AddTaskResponse{})
		require.NoError(t, err, "a configured root is a known future delivery target")
	})

	t.Run("noncanonical configured root", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoPath := setupControlRepo(t)
		cfg := config.DefaultConfig()
		cfg.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
		require.NoError(t, config.SaveConfig(cfg))
		manager, err := NewManager(cfg)
		require.NoError(t, err)

		err = archiveTaskControlServer(manager).AddTask(AddTaskRequest{Task: archiveTargetTask(
			"rootcase", "Mis-cased Root", repoPath, "Root", true,
		)}, &AddTaskResponse{})
		require.Error(t, err, "the ensure loop materializes only the exact reserved root title")
		assert.Contains(t, err.Error(), "use \"root\" exactly")
		_, getErr := task.GetTask("rootcase")
		require.Error(t, getErr, "an impossible reserved target spelling must not commit")
	})

	t.Run("unconfigured root", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		require.False(t, manager.repoRootAgentWillMaterialize(repoID))

		err := archiveTaskControlServer(manager).AddTask(AddTaskRequest{Task: archiveTargetTask(
			"rootnone", "Unconfigured Root", repoPath, session.RootSessionTitle, true,
		)}, &AddTaskResponse{})
		require.Error(t, err, "reserved root cannot be auto-created without root-agent configuration")
		assert.Contains(t, err.Error(), "root")
		assert.Contains(t, err.Error(), "materialize")
		_, getErr := task.GetTask("rootnone")
		require.Error(t, getErr, "an unreachable reserved target must not commit")
	})

	t.Run("deleted root", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoPath := setupControlRepo(t)
		repo, err := config.RepoFromPath(repoPath)
		require.NoError(t, err)
		cfg := config.DefaultConfig()
		cfg.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
		require.NoError(t, config.SaveConfig(cfg))
		manager, err := NewManager(cfg)
		require.NoError(t, err)
		require.NoError(t, task.AddTask(archiveTargetTask(
			"rootlate", "Enable After Delete", repoPath, session.RootSessionTitle, false,
		)))

		_, err = manager.DeleteProject(DeleteProjectRequest{RepoID: repo.ID, RepoPath: repoPath})
		require.NoError(t, err)
		require.False(t, manager.repoRootAgentWillMaterialize(repo.ID), "deleted project roots stay suppressed")
		enabled := true
		err = archiveTaskControlServer(manager).UpdateTask(UpdateTaskRequest{
			ID: "rootlate", Update: task.TaskUpdate{Enabled: &enabled},
		}, &UpdateTaskResponse{})
		require.Error(t, err, "enabling after deletion must not create a permanent reserved-target retry")
		assert.Contains(t, err.Error(), "root")
		assert.Contains(t, err.Error(), "materialize")
		stored, getErr := task.GetTask("rootlate")
		require.NoError(t, getErr)
		assert.False(t, stored.Enabled, "rejected enable must not commit")
	})
}
