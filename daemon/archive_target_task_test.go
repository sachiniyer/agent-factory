package daemon

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archiveTargetTask(id, name, repoPath, target string, enabled bool) task.Task {
	return task.Task{
		ID: id, Name: name, Prompt: "run it", CronExpr: "*/15 * * * *",
		ProjectPath: repoPath, TargetSession: target, Enabled: enabled, CreatedAt: time.Now(),
	}
}

func archiveTaskControlServer(manager *Manager) *controlServer {
	return &controlServer{manager: manager, scheduler: newTaskScheduler()}
}

// TestArchiveSession_RefusesEnabledTargetTasksBeforeMutation pins the lifecycle
// policy for #2646: archiving is refused once, with every blocking automation
// named, rather than succeeding and leaving those tasks in a permanent retry
// loop. Disabled tasks and enabled tasks aimed elsewhere are not blockers.
func TestArchiveSession_RefusesEnabledTargetTasksBeforeMutation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")
	otherRepo := setupTaskRepo(t)

	require.NoError(t, task.AddTask(archiveTargetTask("block001", "Fleet Sweep", repoPath, "worker", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("block002", "Health Watch", repoPath, "worker", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("disabled1", "Paused Task", repoPath, "worker", false)))
	require.NoError(t, task.AddTask(archiveTargetTask("else0001", "Other Target", repoPath, "captain", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("other001", "Other Project", otherRepo, "worker", true)))

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "archive must reject enabled tasks that target the session")
	for _, want := range []string{"Fleet Sweep", "block001", "Health Watch", "block002", "disable or retarget"} {
		assert.Contains(t, err.Error(), want)
	}
	for _, absent := range []string{"Paused Task", "disabled1", "Other Target", "else0001", "Other Project", "other001"} {
		assert.NotContains(t, err.Error(), absent)
	}
	assert.Equal(t, session.LiveReady, inst.GetLiveness(), "rejection must precede the archive fence")
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr, "rejection must not move the worktree")
}

// TestArchiveSession_TaskStoreReadFailureLeavesSessionIntact preserves the
// three-valued outcome: an unreadable tasks store means blockers could not be
// determined, not that none exist. Fail before teardown so repair + retry is
// safe and no automation relationship is guessed away.
func TestArchiveSession_TaskStoreReadFailureLeavesSessionIntact(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")

	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, []byte("{ not json"), 0o600))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "task") && strings.Contains(err.Error(), "determine"),
		"error must name the unknown task relationship, got: %v", err)
	assert.Equal(t, session.LiveReady, inst.GetLiveness())
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr, "unknown task state must not move the worktree")
}

// TestArchiveSession_TaskMutationCannotCrossArchiveFence reproduces the exact
// review race: an AddTask lands after the blocker snapshot but before the
// archive fence. Both operations cannot succeed, or the enabled task is left
// targeting an archived session.
func TestArchiveSession_TaskMutationCannotCrossArchiveFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	server := archiveTaskControlServer(manager)

	checked := make(chan struct{})
	resume := make(chan struct{})
	orig := archiveTargetTasksChecked
	var once sync.Once
	archiveTargetTasksChecked = func() {
		once.Do(func() { close(checked) })
		<-resume
	}
	t.Cleanup(func() { archiveTargetTasksChecked = orig })

	archiveDone := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
		archiveDone <- err
	}()
	<-checked

	addStarted := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		close(addStarted)
		addDone <- server.AddTask(AddTaskRequest{Task: archiveTargetTask(
			"race0001", "Racing Task", repoPath, "worker", true,
		)}, &AddTaskResponse{})
	}()
	<-addStarted
	close(resume)

	archiveErr := <-archiveDone
	addErr := <-addDone
	require.NoError(t, archiveErr, "archive reached preflight first and must win the serialized race")
	require.Error(t, addErr, "the later task mutation must observe the archive fence")
	assert.Contains(t, addErr.Error(), "archiv")
	_, getErr := task.GetTask("race0001")
	require.Error(t, getErr, "the losing task mutation must not commit")
}

// TestTaskMutations_RefuseArchivedTarget pins the other side of the fence:
// once archive wins, later add, enable, and retarget writes must all fail rather
// than create the same permanent delivery loop after the fact.
func TestTaskMutations_RefuseArchivedTarget(t *testing.T) {
	tests := []struct {
		name string
		seed task.Task
		act  func(*controlServer, string) error
	}{
		{
			name: "add",
			act: func(server *controlServer, repoPath string) error {
				return server.AddTask(AddTaskRequest{Task: archiveTargetTask(
					"after001", "After Archive", repoPath, "worker", true,
				)}, &AddTaskResponse{})
			},
		},
		{
			name: "enable",
			seed: archiveTargetTask("enable01", "Enable Later", "", "worker", false),
			act: func(server *controlServer, _ string) error {
				enabled := true
				return server.UpdateTask(UpdateTaskRequest{ID: "enable01", Update: task.TaskUpdate{Enabled: &enabled}}, &UpdateTaskResponse{})
			},
		},
		{
			name: "retarget",
			seed: archiveTargetTask("target01", "Retarget Later", "", "captain", true),
			act: func(server *controlServer, _ string) error {
				target := "worker"
				return server.UpdateTask(UpdateTaskRequest{ID: "target01", Update: task.TaskUpdate{TargetSession: &target}}, &UpdateTaskResponse{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			registerArchivable(t, manager, repoID, repoPath, "worker")
			if tc.seed.ID != "" {
				tc.seed.ProjectPath = repoPath
				require.NoError(t, task.AddTask(tc.seed))
			}
			_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
			require.NoError(t, err)
			before, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)

			err = tc.act(archiveTaskControlServer(manager), repoPath)
			require.Error(t, err, "an enabled task must not be written against an archived target")
			assert.Contains(t, err.Error(), "archiv")
			after, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)
			assert.Equal(t, before, after, "rejected mutation must leave the task store byte-semantically unchanged")
		})
	}
}

// TestDeleteProject_TaskBlockerIsPreflight preserves the project's lifecycle
// configuration when a predictable blocker makes deletion impossible. The
// root-agent opt-in and in-memory respawn policy must be unchanged.
func TestDeleteProject_TaskBlockerIsPreflight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")
	require.NoError(t, task.AddTask(archiveTargetTask("delete01", "Deletion Blocker", repoPath, "worker", true)))

	seed := config.DefaultConfig()
	seed.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
	require.NoError(t, config.SaveConfig(seed))

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err)
	assert.Empty(t, result.Archived)
	assert.Contains(t, err.Error(), "Deletion Blocker")

	cfg, loadErr := config.LoadConfig()
	require.NoError(t, loadErr)
	assert.Contains(t, cfg.RootAgents, repoPath, "task refusal must precede durable root-agent removal")
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[repoID]
	manager.mu.Unlock()
	assert.False(t, suppressed, "task refusal must precede in-memory root-agent suppression")
	assert.Equal(t, session.LiveReady, inst.GetLiveness())
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr)
}
