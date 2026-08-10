package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCleanupReadyGhost(t *testing.T, title string) (*Manager, string) {
	t.Helper()
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
		ID: title + "-id", Title: title, Path: repoPath,
		Status: session.Archived, Liveness: session.LiveArchived, BackendType: "local",
		Worktree: session.GitWorktreeData{
			RepoPath: repoPath, WorktreePath: archive, SessionName: title,
			BranchName: "af/" + title, BranchCreatedByUs: &branchCreated,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State: sessiongit.RelocationRecoveryCleanupReady, IdentityKnown: true,
				Device: uint64(stat.Dev), Inode: uint64(stat.Ino), FileType: uint32(stat.Mode & syscall.S_IFMT),
				OriginalExternalWorktree: &originalExternal, OriginalBranchCreatedByUs: &originalBranchCreated,
				OriginalStartupStateUnknown: &originalStartupUnknown,
			},
		},
	}))
	require.NoError(t, os.RemoveAll(repoPath))
	failLoadFor(t, title)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	return manager, repo.ID
}

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

func TestRestoreArchived_NonGitOriginCreatesCleanupIdentityBeforePathResolution(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "non-git-origin")
	require.NoError(t, os.RemoveAll(filepath.Join(repoPath, ".git")),
		"make the still-present origin pathname conclusively non-Git")

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "non-git-origin", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin repo "+repoPath+" is gone")
	assert.True(t, exists(archivedPath), "failed restore must leave the archive intact")
	assert.Equal(t, session.Archived, inst.GetStatus())
	recovery := recordFor(t, repoID, "non-git-origin").Worktree.RelocationRecovery
	require.NotNil(t, recovery,
		"the pre-path guard must create cleanup authorization for a non-Git origin")
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
	require.NoError(t, exec.Command("git", "init", repoPath).Run(),
		"bring a valid origin repository back before kill")

	_, err = manager.KillSession(KillSessionRequest{Title: "repo-returned", RepoID: repoID})
	require.Error(t, err)
	assert.False(t, inst.UserKilled(), "the returned-origin guard must run before the kill tombstone")
	assert.True(t, exists(archivedPath), "a refused kill must leave the archive intact")
	assert.NotNil(t, recordFor(t, repoID, "repo-returned").Worktree.RelocationRecovery)
}

func TestKillSession_GhostCleanupReadyRevalidatesBeforeTombstone(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-ready")
	cleanupCalls := stubGhostWorktree(t, sessiongit.CleanupSettled, nil)

	_, err := manager.KillSession(KillSessionRequest{Title: "ghost-ready", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, 1, *cleanupCalls, "an admitted cleanup-ready ghost must reach its claimed cleanup")
	assert.Nil(t, recordFor(t, repoID, "ghost-ready"), "successful ghost cleanup must consume the durable row")
}

func TestKillSession_GhostCleanupTimeoutPersistsStallAndBlocksSameProcessRetry(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-stall")
	previousCleanup := ghostCleanupWorktree
	cleanupCalls := 0
	ghostCleanupWorktree = func(data *session.InstanceData, _ string) (sessiongit.CleanupState, error, <-chan error) {
		cleanupCalls++
		data.Worktree.RelocationRecovery.State = sessiongit.RelocationRecoveryCleanupStalled
		return sessiongit.CleanupStateUnknown, context.DeadlineExceeded, nil
	}
	t.Cleanup(func() { ghostCleanupWorktree = previousCleanup })

	_, err := manager.KillSession(KillSessionRequest{Title: "ghost-stall", RepoID: repoID})
	require.Error(t, err)
	record := recordFor(t, repoID, "ghost-stall")
	require.NotNil(t, record)
	assert.True(t, record.UserKilled)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupStalled, record.Worktree.RelocationRecovery.State,
		"the temporary ghost handle's process-epoch stall must become durable")

	_, err = manager.KillSession(KillSessionRequest{Title: "ghost-stall", RepoID: repoID})
	require.Error(t, err)
	assert.Equal(t, 1, cleanupCalls, "a same-process retry must not launch another deletion worker")
}

func TestLateGhostCleanup_DeleteFailureIsRetriedFromDefinitiveSuccess(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-late-success")
	key := daemonInstanceKey(repoID, "ghost-late-success")
	stableID := "ghost-late-success-id"
	manager.markGhostCleanupStalled(key, stableID)

	previousDelete := lateGhostDeleteSessionRecord
	previousInterval := lateGhostCleanupRetryInterval
	var attempts atomic.Int32
	retried := make(chan struct{})
	lateGhostCleanupRetryInterval = 5 * time.Millisecond
	lateGhostDeleteSessionRecord = func(_ *Manager, _, _, _ string, _ error) (bool, error) {
		if attempts.Add(1) == 1 {
			return false, errors.New("transient instances lock failure")
		}
		retried <- struct{}{}
		return true, nil
	}
	t.Cleanup(func() {
		lateGhostDeleteSessionRecord = previousDelete
		lateGhostCleanupRetryInterval = previousInterval
	})

	lateResult := make(chan error, 1)
	lateResult <- nil
	manager.reconcileLateGhostCleanup(repoID, "ghost-late-success", key, stableID, lateResult)

	select {
	case <-retried:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("late cleanup completion was discarded after one delete failure; attempts=%d", attempts.Load())
	}
	require.Eventually(t, func() bool {
		return !manager.ghostCleanupStallActive(key, stableID)
	}, 250*time.Millisecond, time.Millisecond, "a successful retry must release the process-epoch fence")
}

func TestKillSession_GhostCleanupSettledTailFailureRetriesFinalization(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-sync-success")
	stubGhostTmux(t, tmux.PaneStateKnown, nil)

	previousCleanup := ghostCleanupWorktree
	var releaseLock func()
	ghostCleanupWorktree = func(data *session.InstanceData, _ string) (sessiongit.CleanupState, error, <-chan error) {
		// Mirror the real descriptor cleanup's definitive success: the archive is
		// gone and the temporary in-memory recovery handle has been consumed.
		data.Worktree.RelocationRecovery = nil
		path, err := config.RepoInstancesPath(repoID)
		require.NoError(t, err)
		releaseLock = holdFileLock(t, path)
		return sessiongit.CleanupSettled, nil, nil
	}
	t.Cleanup(func() {
		ghostCleanupWorktree = previousCleanup
		if releaseLock != nil {
			releaseLock()
		}
	})

	previousDeleteTimeout := session.InstanceDeleteLockTimeout
	session.InstanceDeleteLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { session.InstanceDeleteLockTimeout = previousDeleteTimeout })
	previousRetryInterval := lateGhostCleanupRetryInterval
	lateGhostCleanupRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() { lateGhostCleanupRetryInterval = previousRetryInterval })

	_, err := manager.KillSession(KillSessionRequest{Title: "ghost-sync-success", RepoID: repoID})
	require.ErrorIs(t, err, config.ErrLockTimeout)
	require.NotNil(t, releaseLock, "the tail failure must happen after descriptor cleanup succeeded")
	releaseLock()

	require.Eventually(t, func() bool {
		return recordFor(t, repoID, "ghost-sync-success") == nil
	}, 500*time.Millisecond, 5*time.Millisecond,
		"definitive synchronous cleanup must keep retrying its editor/record tail")
}
