package daemon

import (
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreArchived_RepoGoneKeepsIdentityUntilKill(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "repo-gone-authorization")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{
		Title: "repo-gone-authorization", RepoID: repoID,
	})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "repo-gone-authorization").Worktree.RelocationRecovery)
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{
		Title: "repo-gone-authorization", RepoID: repoID,
	})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "failed restore must leave the archive intact")
	recovery := recordFor(t, repoID, "repo-gone-authorization").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "repo-gone restore must preserve a durable cleanup authorization")
	assert.Equal(t, sessiongit.RelocationRecoveryState("cleanup_ready"), recovery.State)
	assert.True(t, recovery.IdentityKnown)

	_, err = manager.KillSession(KillSessionRequest{
		Title: "repo-gone-authorization", RepoID: repoID,
	})
	require.Error(t, err, "slice 1 must refuse cleanup-ready kill before pane teardown")
	assert.False(t, inst.UserKilled(), "refusal must happen before the kill tombstone")
	assert.True(t, exists(archivedPath), "refused kill must leave the archive intact")
}
