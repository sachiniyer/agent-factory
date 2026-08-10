package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archivedInstanceWithRecoveryClaim(t *testing.T, title string) (*Manager, string, string, *session.Instance, string) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	info, err := os.Stat(archivedPath)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.NoError(t, gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))
	return manager, repoID, repoPath, inst, archivedPath
}

func TestRestoreArchived_RepoDisappearsAfterGuardKeepsCleanupIdentity(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "late-gone")

	previous := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		require.NoError(t, os.RemoveAll(repoPath), "remove origin after the early guard")
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = previous })

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "late-gone", RepoID: repoID})
	require.Error(t, err)
	assert.ErrorIs(t, err, sessiongit.ErrRepoGone)
	assert.True(t, exists(archivedPath), "the failed restore must leave the archive intact")
	assert.Equal(t, session.Archived, inst.GetStatus())
	recovery := recordFor(t, repoID, "late-gone").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "the authoritative repo-gone exit must leave a durable cleanup handle")
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.State)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_RepoReturnsBeforeKillRefusesBeforeTombstone(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "repo-returned")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "repo-returned", RepoID: repoID})
	require.Error(t, err)
	recovery := recordFor(t, repoID, "repo-returned").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.State)
	require.NoError(t, os.MkdirAll(repoPath, 0o755), "bring the origin pathname back before kill")

	_, err = manager.KillSession(KillSessionRequest{Title: "repo-returned", RepoID: repoID})
	require.Error(t, err)
	assert.False(t, inst.UserKilled(), "the returned-origin guard must run before the kill tombstone")
	assert.True(t, exists(archivedPath), "a refused kill must leave the archive intact")
	assert.NotNil(t, recordFor(t, repoID, "repo-returned").Worktree.RelocationRecovery)
}

func TestKillSession_GhostCleanupReadyRevalidatesBeforeTombstone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	archive := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, os.Mkdir(archive, 0o755))
	info, err := os.Stat(archive)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	originalExternal := false
	originalBranchCreated := true
	originalStartupUnknown := false
	branchCreated := true
	require.NoError(t, appendInstanceData(repo.ID, session.InstanceData{
		ID: "ghost-ready-id", Title: "ghost-ready", Path: repoPath,
		Status: session.Archived, Liveness: session.LiveArchived, BackendType: "local",
		Worktree: session.GitWorktreeData{
			RepoPath: repoPath, WorktreePath: archive, SessionName: "ghost-ready",
			BranchName: "af/ghost-ready", BranchCreatedByUs: &branchCreated,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State: sessiongit.RelocationRecoveryCleanupReady, IdentityKnown: true,
				Device: uint64(stat.Dev), Inode: uint64(stat.Ino), FileType: uint32(stat.Mode & syscall.S_IFMT),
				OriginalExternalWorktree: &originalExternal, OriginalBranchCreatedByUs: &originalBranchCreated,
				OriginalStartupStateUnknown: &originalStartupUnknown,
			},
		},
	}))
	require.NoError(t, os.RemoveAll(repoPath))
	failLoadFor(t, "ghost-ready")
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cleanupCalls := stubGhostWorktree(t, sessiongit.CleanupSettled, nil)

	_, err = manager.KillSession(KillSessionRequest{Title: "ghost-ready", RepoID: repo.ID})
	require.NoError(t, err)
	assert.Equal(t, 1, *cleanupCalls, "an admitted cleanup-ready ghost must reach its claimed cleanup")
	assert.Nil(t, recordFor(t, repo.ID, "ghost-ready"), "successful ghost cleanup must consume the durable row")
}
