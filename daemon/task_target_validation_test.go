package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveSession_UnresolvedLegacyTaskScopeFailsClosed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")
	bound := filepath.Join(t.TempDir(), "bound")
	require.NoError(t, os.Symlink(repoPath, bound))
	legacy := archiveTargetTask("unknown2", "Unknown Binding", bound, "worker", true)
	raw, err := json.Marshal([]task.Task{legacy})
	require.NoError(t, err)
	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, raw, 0o600))
	require.NoError(t, os.Remove(bound))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "archive must not infer that an unresolved legacy task belongs elsewhere")
	assert.Contains(t, err.Error(), "could not determine")
	assert.Contains(t, err.Error(), "unknown2")
	assert.Equal(t, session.LiveReady, inst.GetLiveness(), "unknown scope must fail before the archive fence")
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr, "unknown scope must not move the worktree")
}

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

func TestTaskMutations_LegacyEnablePersistsResolvedRepoBinding(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	otherRepoPath := setupTaskRepo(t)
	bound := filepath.Join(t.TempDir(), "bound")
	require.NoError(t, os.Symlink(repoPath, bound))
	legacy := archiveTargetTask("legacy02", "Legacy Binding", bound, "worker", false)
	raw, err := json.Marshal([]task.Task{legacy})
	require.NoError(t, err)
	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, raw, 0o600))

	enabled := true
	err = archiveTaskControlServer(manager).UpdateTask(UpdateTaskRequest{
		ID: legacy.ID, Update: task.TaskUpdate{Enabled: &enabled},
	}, &UpdateTaskResponse{})
	require.NoError(t, err, "a resolvable legacy target may be enabled")
	stored, err := task.GetTask(legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, repoID, stored.RepoID, "the identity lent to validation must be committed with the update")

	require.NoError(t, os.Remove(bound))
	require.NoError(t, os.Symlink(otherRepoPath, bound))
	otherRepo, err := config.RepoFromPath(otherRepoPath)
	require.NoError(t, err)
	otherTasks, err := task.LoadTasksForRepoID(otherRepo.ID)
	require.NoError(t, err)
	assert.Empty(t, otherTasks, "a later path rebind must not move the updated task to another project")
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

	t.Run("live unconfigured root", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		registerArchivable(t, manager, repoID, repoPath, session.RootSessionTitle)
		require.False(t, manager.repoRootAgentWillMaterialize(repoID))

		err := archiveTaskControlServer(manager).AddTask(AddTaskRequest{Task: archiveTargetTask(
			"rootlive", "Unmanaged Live Root", repoPath, session.RootSessionTitle, true,
		)}, &AddTaskResponse{})
		require.Error(t, err, "a live reserved target without self-healing would become a permanent retry after it disappears")
		assert.Contains(t, err.Error(), "materialize")
		_, getErr := task.GetTask("rootlive")
		require.Error(t, getErr, "an unmanaged reserved target must not commit")
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

func TestTaskArming_RootPolicyDriftFailsBeforeScheduling(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	require.NoError(t, task.AddTask(archiveTargetTask(
		"rootdrift", "Root Policy Drift", repoPath, session.RootSessionTitle, true,
	)), "seed a task accepted while root policy was enabled by the previous daemon")
	require.NoError(t, task.AddTask(archiveTargetTask(
		"safearm1", "Safe Automation", repoPath, "", true,
	)))

	scheduler := newTaskScheduler()
	watchers := newWatcherSupervisor()
	err := armTaskAutomation(manager, scheduler, watchers)
	require.Error(t, err, "startup must not arm a root-target task after root policy is disabled")
	assert.Contains(t, err.Error(), "rootdrift")
	assert.Contains(t, err.Error(), "materialize")
	assert.NotContains(t, scheduler.scheduledTaskIDs(), "rootdrift", "unsafe root automation must stay unarmed")
	assert.Contains(t, scheduler.scheduledTaskIDs(), "safearm1", "one unsafe task must not suppress unrelated automation")
	assert.Empty(t, watchers.watchingTaskIDs(), "failed startup preflight must leave watch automation unarmed")
}

func TestTaskArming_RefusedWatchTaskKeepsDurableQueue(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	unsafe := watchTask("rootqueue", "sleep 60", repoPath)
	unsafe.TargetSession = session.RootSessionTitle
	require.NoError(t, task.AddTask(unsafe),
		"seed a task accepted while root-agent policy was enabled by the previous daemon")

	watchers, _ := newTestSupervisor(t, task.LoadTasks)
	queueDir, err := watchers.queueDir()
	require.NoError(t, err)
	require.NoError(t, newEventQueue(queueDir, unsafe.ID).enqueue("pending"))

	scheduler := newTaskScheduler()
	scheduler.controlMu.Lock()
	refused, reloadErr := reloadTaskAutomation(manager, scheduler, watchers)
	scheduler.controlMu.Unlock()
	require.NoError(t, reloadErr)
	require.NotEmpty(t, refused, "unsafe task must be refused instead of armed")
	_, statErr := os.Stat(filepath.Join(queueDir, unsafe.ID+".jsonl"))
	require.NoError(t, statErr,
		"a refused task still exists in tasks.json, so its repairable backlog must not be treated as orphaned")
}
