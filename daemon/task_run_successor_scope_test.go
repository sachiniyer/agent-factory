package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The later-run scan reads every session file it can, but an unreadable one
// blocks it only if that project could hold a run of the task (#4224 review).
//
// At 108540d8 the first call below fails: any unreadable file anywhere returned
// "cannot inspect persisted task-run successors while restoring repo …".
func TestSuccessorScanFailsClosedOnlyForProjectsThatCanHoldTheTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seedTaskRunStore(t, "own-repo", session.InstanceData{
		ID: "run-1", Title: "run-1", Path: "/repos/own", Program: "claude", TaskID: "scope001",
	})
	unreadablePath := seedUnreadableRepoHolding(t, "unreadable-repo", "elsewhere", "/repos/unreadable")
	garbledPath, err := config.RepoInstancesPath("garbled-repo")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(garbledPath), 0o755))
	require.NoError(t, os.WriteFile(garbledPath, []byte("not json"), 0o600))

	rows, err := persistedTaskRunsForAttribution(map[string]bool{"own-repo": true})
	require.NoError(t, err, "projects that cannot hold this task's runs must not block the scan")
	require.Len(t, rows, 1)
	assert.Equal(t, "run-1", rows[0].ID, "readable rows are still returned beside the skipped files")

	_, err = persistedTaskRunsForAttribution(map[string]bool{"own-repo": true, "unreadable-repo": true})
	require.Error(t, err, "an unreadable file that could hold a run of this task fails closed")
	assertNamesFileAndCause(t, err, unreadablePath)

	_, err = persistedTaskRunsForAttribution(map[string]bool{"own-repo": true, "garbled-repo": true})
	require.Error(t, err, "an undecodable file that could hold a run of this task fails closed")
	assert.Contains(t, err.Error(), "garbled-repo")
}

// The projects that can hold a task's runs are the settled session's own and
// the task's current one, including a pre-RepoID row's resolved path.
func TestTaskRunStoreReposNamesSessionAndTaskProjects(t *testing.T) {
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	assert.Equal(t, map[string]bool{"session-repo": true, "task-repo": true},
		taskRunStoreRepos("session-repo", task.Task{RepoID: "task-repo", ProjectPath: repoPath}))
	assert.Equal(t, map[string]bool{"session-repo": true, repo.ID: true},
		taskRunStoreRepos("session-repo", task.Task{ProjectPath: repoPath}),
		"a row written before repo_id existed is placed by its path")
	assert.Equal(t, map[string]bool{"session-repo": true},
		taskRunStoreRepos("session-repo", task.Task{}))
}

// An unreadable session file in an unrelated project must neither keep the
// interruption from being recorded nor, through the pending marker it would
// leave set, keep the session from being deleted (#4224 review).
//
// At 108540d8 the scan fails on the unrelated file, so the status assertion
// reads "started", the marker stays set, and deleteSessionRecord refuses.
func TestInterruptionOutcomeIgnoresUnreadableStoreInUnrelatedProject(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("scope002", repoPath))
	runAt := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	inst := publishedTaskRun(t, repoPath, tsk, "unrelated-unreadable", 1, runAt)
	seedTaskRunStore(t, repoID, inst.ToInstanceData())
	key := daemonInstanceKey(repoID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	seedUnreadableRepoHolding(t, "unrelated-repo", "someone-else", "/repos/unrelated")

	interruptTaskRunRuntime(t, manager, repoID, key, inst)

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusInterrupted, got.LastRunStatus,
		"an unrelated project's unreadable file cannot hold a run of this task")
	assert.NotContains(t, logs.warnings.String(), "will retry")
	_, pending := inst.PendingTaskRunInterruption()
	assert.False(t, pending, "the recorded outcome retires the retry marker")
	manager.mu.Lock()
	_, owed := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	assert.False(t, owed)

	deleted, err := manager.deleteSessionRecord(repoID, inst.Title, inst.ID, nil, inst.ToInstanceData())
	require.NoError(t, err, "an unrelated project's file must not make this session undeletable")
	assert.True(t, deleted)
}

// The narrowing keeps the project the task has moved to. A later run of the
// task is created there, so an unreadable file in that project still fails
// closed and the outcome stays owed.
//
// This passes at 108540d8, where every unreadable file failed closed. It fails
// if taskRunStoreRepos leaves out the task's current project.
func TestInterruptionOutcomeWaitsForUnreadableStoreInTaskProject(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	movedPath := setupControlRepo(t)
	moved, err := config.RepoFromPath(movedPath)
	require.NoError(t, err)
	require.NotEqual(t, repoID, moved.ID, "precondition: the task now lives in another project")
	tsk := addStatusTestTask(t, enabledCronTask("scope003", movedPath))
	require.Equal(t, moved.ID, tsk.RepoID, "precondition: the task is bound to the project it moved to")

	runAt := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	inst := publishedTaskRun(t, repoPath, tsk, "moved-task-run", 1, runAt)
	seedTaskRunStore(t, repoID, inst.ToInstanceData())
	key := daemonInstanceKey(repoID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	seedUnreadableRepoHolding(t, moved.ID, "later-run", movedPath)

	interruptTaskRunRuntime(t, manager, repoID, key, inst)

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.RunStatusStarted, got.LastRunStatus,
		"an unreadable file in the task's own project may hold its later run")
	assert.Contains(t, logs.warnings.String(), "will retry")
	manager.mu.Lock()
	entry, owed := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	require.True(t, owed)
	assert.NotNil(t, entry.interruptedTaskRun)
}
