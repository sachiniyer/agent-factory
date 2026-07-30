package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

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
