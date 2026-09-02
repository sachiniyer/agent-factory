package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type afterKillBackend struct {
	session.Backend
	afterKill func()
}

func (b *afterKillBackend) Kill(inst *session.Instance, trustLiveGeneration bool) error {
	if err := b.Backend.Kill(inst, trustLiveGeneration); err != nil {
		return err
	}
	b.afterKill()
	return nil
}

func TestFinishUserKill_LiveCleanupPersistsFinalizationBeforeTail(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "live-retry-crash-window")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "live-retry-crash-window", RepoID: repoID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "origin repo "+repoPath+" is gone")
	recovery := recordFor(t, repoID, "live-retry-crash-window").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)

	// A previous explicit kill can leave this durable tombstone when pane teardown
	// is unknown. The poll then reaches finishUserKill with the same live instance
	// and cleanup-ready handle, so this is a reachable destructive retry rather
	// than a synthetic direct call into worktree cleanup.
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))
	instancesPath, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	var releaseLock func()
	inst.SetBackend(&afterKillBackend{
		Backend: &session.LocalBackend{},
		afterKill: func() {
			// Block the post-Kill checkpoint after descriptor cleanup has returned.
			// Only the callback inside the descriptor fence can have reached disk.
			releaseLock = holdFileLock(t, instancesPath)
		},
	})
	t.Cleanup(func() {
		if releaseLock != nil {
			releaseLock()
		}
	})

	previousPersistTimeout := config.RepoInstancesLockTimeout
	config.RepoInstancesLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { config.RepoInstancesLockTimeout = previousPersistTimeout })

	manager.finishUserKill(repoID, inst)
	require.NotNil(t, releaseLock, "the retry must reach the fallible tail after descriptor cleanup")
	assert.False(t, exists(archivedPath), "descriptor cleanup must finish before the tail checkpoint fails")

	retained := recordFor(t, repoID, "live-retry-crash-window")
	require.NotNil(t, retained)
	require.NotNil(t, retained.Worktree.RelocationRecovery)
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, retained.Worktree.RelocationRecovery.State)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupFinalizing, retained.Worktree.RelocationRecovery.CleanupLifecycle,
		"the automatic retry must durably enter the finalization fence before unlinking the archive root")
}

func TestKillSession_LiveCleanupSettlesDurablyBeforeTailFailure(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "live-tail")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "live-tail", RepoID: repoID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "origin repo "+repoPath+" is gone")
	recovery := recordFor(t, repoID, "live-tail").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)

	key := daemonInstanceKey(repoID, "live-tail")
	inst.SetBackend(&afterKillBackend{
		Backend: &session.LocalBackend{},
		afterKill: func() {
			manager.vscode.mu.Lock()
			require.True(t, manager.vscode.reserveReconcileLocked(key))
			manager.vscode.mu.Unlock()
		},
	})
	_, err = manager.KillSession(KillSessionRequest{Title: "live-tail", RepoID: repoID})
	require.Error(t, err, "the injected second editor fence must retain the tombstoned row")
	assert.False(t, exists(archivedPath), "descriptor cleanup must have completed before the tail failure")

	retained := recordFor(t, repoID, "live-tail")
	require.NotNil(t, retained)
	assert.Nil(t, retained.Worktree.RelocationRecovery,
		"the durable tombstone must record descriptor completion before a fallible tail step")

	manager.vscode.releaseReconcile(key)
	restarted, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	_, err = restarted.KillSession(KillSessionRequest{Title: "live-tail", RepoID: repoID})
	require.NoError(t, err, "restart must finish the tombstone without reinterpreting the absent archive")
	assert.Nil(t, recordFor(t, repoID, "live-tail"))
}
