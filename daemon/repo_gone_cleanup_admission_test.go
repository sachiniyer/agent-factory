package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
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
	recovery := preparedCleanupRecovery(t, repoPath, archive, title)
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
				Device: recovery.Device, Inode: recovery.Inode, FileType: recovery.FileType,
				CleanupGeneration:        recovery.CleanupGeneration,
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

func preparedCleanupRecovery(t *testing.T, repoPath, archive, title string) sessiongit.RelocationRecovery {
	t.Helper()
	info, err := os.Stat(archive)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	gw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, archive, title, "af/"+title, "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)
	require.NoError(t, gw.PrepareRelocationClaimForCleanup(claim))
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok)
	return recovery
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

type afterKillBackend struct {
	session.Backend
	afterKill func()
}

func (b *afterKillBackend) Kill(inst *session.Instance) error {
	if err := b.Backend.Kill(inst); err != nil {
		return err
	}
	b.afterKill()
	return nil
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

func TestRestoreArchived_OriginProbeFailurePersistsUnresolvedClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "probe-unknown")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "probe-unknown", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "probe-unknown").Worktree.RelocationRecovery,
		"precondition: an ordinary successful archive starts without a recovery record")
	t.Setenv("PATH", t.TempDir())

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "probe-unknown", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "unknown origin probe must leave the archive intact")
	recovery := recordFor(t, repoID, "probe-unknown").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "record-free claim must become a durable non-destructive fence")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
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
	ghostCleanupWorktree = func(data *session.InstanceData, _ string, _ func(*session.InstanceData) error) (sessiongit.CleanupState, error, <-chan error) {
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
	retained := filepath.Join(t.TempDir(), ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(retained, 0o755))
	retainedInfo, err := os.Stat(retained)
	require.NoError(t, err)
	retainedStat, ok := retainedInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	record := recordFor(t, repoID, "ghost-sync-success")
	record.ArchiveReport = &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: retained, IdentityKnown: true,
		Device: uint64(retainedStat.Dev), Inode: uint64(retainedStat.Ino),
		FileType: uint32(retainedStat.Mode & syscall.S_IFMT),
	}}}
	require.NoError(t, persistInstanceData(repoID, *record))
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale,
		recordFor(t, repoID, "ghost-sync-success").Worktree.RelocationRecovery.State,
		"precondition: archive rollback projection must hide cleanup_ready")

	previousCleanup := ghostCleanupWorktree
	var releaseLock func()
	ghostCleanupWorktree = func(data *session.InstanceData, _ string, _ func(*session.InstanceData) error) (sessiongit.CleanupState, error, <-chan error) {
		// Mirror the real descriptor cleanup's definitive success: the archive is
		// gone and the temporary in-memory recovery handle has been consumed.
		data.Worktree.RelocationRecovery = nil
		data.ArchiveReport = nil
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

	_, err = manager.KillSession(KillSessionRequest{Title: "ghost-sync-success", RepoID: repoID})
	require.ErrorIs(t, err, config.ErrLockTimeout)
	require.NotNil(t, releaseLock, "the tail failure must happen after descriptor cleanup succeeded")
	releaseLock()

	require.Eventually(t, func() bool {
		return recordFor(t, repoID, "ghost-sync-success") == nil
	}, 500*time.Millisecond, 5*time.Millisecond,
		"definitive synchronous cleanup must keep retrying its editor/record tail")
}

func TestKillSession_GhostCleanupPersistsFinalizationBeforeTail(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-crash-window")
	stubGhostTmux(t, tmux.PaneStateKnown, nil)
	retained := filepath.Join(t.TempDir(), ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(retained, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(retained, "private.txt"), []byte("retained"), 0o644))
	retainedInfo, err := os.Stat(retained)
	require.NoError(t, err)
	retainedStat, ok := retainedInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	record := recordFor(t, repoID, "ghost-crash-window")
	require.NotNil(t, record)
	record.ArchiveReport = &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: retained, IdentityKnown: true,
		Device: uint64(retainedStat.Dev), Inode: uint64(retainedStat.Ino),
		FileType: uint32(retainedStat.Mode & syscall.S_IFMT),
	}}}
	require.NoError(t, persistInstanceData(repoID, *record))
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale,
		recordFor(t, repoID, "ghost-crash-window").Worktree.RelocationRecovery.State,
		"precondition: storage must project cleanup_ready behind the archive rollback fence")

	// Drive the real descriptor cleanup, then hold the instances lock before the
	// ordinary row-delete tail. This models a daemon dying after the archive root
	// has been removed: only state persisted from inside the descriptor fence can
	// survive that boundary.
	previousCleanup := ghostCleanupWorktree
	var releaseLock func()
	ghostCleanupWorktree = func(data *session.InstanceData, title string, checkpoint func(*session.InstanceData) error) (sessiongit.CleanupState, error, <-chan error) {
		state, cleanupErr, lateResult := previousCleanup(data, title, checkpoint)
		if state == sessiongit.CleanupSettled && cleanupErr == nil && lateResult == nil {
			path, err := config.RepoInstancesPath(repoID)
			require.NoError(t, err)
			releaseLock = holdFileLock(t, path)
		}
		return state, cleanupErr, lateResult
	}
	t.Cleanup(func() {
		ghostCleanupWorktree = previousCleanup
		if releaseLock != nil {
			releaseLock()
		}
	})

	// Keep the first manager's process-local finalizer from masking what a real
	// restart would read from disk.
	previousLateDelete := lateGhostDeleteSessionRecord
	releaseOldFinalizer := make(chan struct{})
	oldFinalizerStarted := make(chan struct{})
	oldFinalizerDone := make(chan struct{})
	oldFinalizerReleased := false
	lateGhostDeleteSessionRecord = func(*Manager, string, string, string, error) (bool, error) {
		close(oldFinalizerStarted)
		<-releaseOldFinalizer
		close(oldFinalizerDone)
		return false, nil
	}
	t.Cleanup(func() {
		if !oldFinalizerReleased {
			close(releaseOldFinalizer)
		}
		select {
		case <-oldFinalizerStarted:
			select {
			case <-oldFinalizerDone:
			case <-time.After(time.Second):
			}
		default:
		}
		lateGhostDeleteSessionRecord = previousLateDelete
	})

	previousDeleteTimeout := session.InstanceDeleteLockTimeout
	session.InstanceDeleteLockTimeout = 25 * time.Millisecond
	t.Cleanup(func() { session.InstanceDeleteLockTimeout = previousDeleteTimeout })

	_, err = manager.KillSession(KillSessionRequest{Title: "ghost-crash-window", RepoID: repoID})
	require.ErrorIs(t, err, config.ErrLockTimeout)
	require.NotNil(t, releaseLock, "the tail failure must happen after descriptor cleanup succeeded")
	record = recordFor(t, repoID, "ghost-crash-window")
	require.NotNil(t, record)
	require.NotNil(t, record.Worktree.RelocationRecovery)
	require.Equal(t, sessiongit.RelocationRecoveryState("cleanup_finalizing"), record.Worktree.RelocationRecovery.State,
		"the durable row must record that descriptor cleanup entered its finalization fence")
	assert.True(t, record.ArchiveReport == nil || record.ArchiveReport.Empty(),
		"the same checkpoint must retire the consumed retained-tree handle")

	// The session/git finalization-retry tests load this persisted state into a
	// fresh worktree and prove both absent-root completion and replacement safety.
	releaseLock()
	releaseLock = nil
	close(releaseOldFinalizer)
	oldFinalizerReleased = true
}

func TestGhostCleanupWorktree_DetachesCheckpointBaseline(t *testing.T) {
	_, repoID := newCleanupReadyGhost(t, "ghost-detached-baseline")
	data := *recordFor(t, repoID, "ghost-detached-baseline")
	originalBranch := data.Worktree.BranchName
	entered := make(chan struct{})
	release := make(chan struct{})
	previousBeforeSnapshot := beforeGhostCheckpointSnapshot
	beforeGhostCheckpointSnapshot = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { beforeGhostCheckpointSnapshot = previousBeforeSnapshot })

	type cleanupResult struct {
		state sessiongit.CleanupState
		err   error
	}
	result := make(chan cleanupResult, 1)
	checkpoint := make(chan session.InstanceData, 1)
	go func() {
		state, cleanupErr, _ := ghostCleanupWorktree(&data, data.Title, func(snapshot *session.InstanceData) error {
			checkpoint <- *snapshot
			return errors.New("stop after checkpoint capture")
		})
		result <- cleanupResult{state: state, err: cleanupErr}
	}()
	<-entered
	data.Worktree.BranchName = "mutated-after-cleanup-started"
	close(release)

	snapshot := <-checkpoint
	if snapshot.Worktree.BranchName != originalBranch {
		t.Fatalf("late checkpoint read mutable caller data: got %q want %q", snapshot.Worktree.BranchName, originalBranch)
	}
	completed := <-result
	require.Error(t, completed.err)
	assert.Equal(t, sessiongit.CleanupStateUnknown, completed.state)
}

func TestFinishUserKill_LiveCleanupPersistsFinalizationBeforeTail(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "live-retry-crash-window")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "live-retry-crash-window", RepoID: repoID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "origin repo "+repoPath+" is gone")
	recovery := recordFor(t, repoID, "live-retry-crash-window").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.State)

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
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupFinalizing, retained.Worktree.RelocationRecovery.State,
		"the automatic retry must durably enter the finalization fence before unlinking the archive root")
}

func TestValidateGhostCleanupAdmission_RestoresArchiveRollbackRecoveryFirst(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "archive")
	require.NoError(t, os.Mkdir(archive, 0o755))
	recovery := preparedCleanupRecovery(t, filepath.Join(root, "missing-repo"), archive, "rollback-cleanup")
	originalExternal := false
	originalBranchCreated := true
	originalStartupUnknown := false
	branchCreated := true
	data := session.InstanceData{
		ID: "rollback-cleanup-id", Title: "rollback-cleanup", Status: session.Archived,
		Liveness: session.LiveArchived, BackendType: "local",
		Worktree: session.GitWorktreeData{
			RepoPath: filepath.Join(root, "missing-repo"), WorktreePath: archive,
			SessionName: "rollback-cleanup", BranchName: "af/rollback-cleanup",
			BranchCreatedByUs: &branchCreated,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State: sessiongit.RelocationRecoveryCleanupReady, IdentityKnown: true,
				Device: recovery.Device, Inode: recovery.Inode, FileType: recovery.FileType,
				CleanupGeneration:        recovery.CleanupGeneration,
				OriginalExternalWorktree: &originalExternal, OriginalBranchCreatedByUs: &originalBranchCreated,
				OriginalStartupStateUnknown: &originalStartupUnknown,
			},
		},
		ArchiveReport: &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
			Path: filepath.Join(root, "retained-source"), IdentityKnown: true,
			Device: 17, Inode: 23, FileType: uint32(syscall.S_IFDIR),
			Skipped: []sessiongit.ArchiveSkippedEntry{{Path: "private/work", Reason: sessiongit.ArchiveSkipPermissionDenied}},
		}}},
	}
	stored := data.ForStorage()
	require.NotNil(t, stored.Worktree.RelocationRecovery)
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale, stored.Worktree.RelocationRecovery.State,
		"precondition: the old-reader projection must hide cleanup_ready")

	require.NoError(t, validateGhostWorktreeDestructionAdmission(&stored),
		"current admission must decode cleanup_ready before interpreting the projected state")
}

func TestLateGhostCleanup_SuccessCompletesRootKill(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, session.RootSessionTitle)
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	const stableID = "root-id"

	previousDelete := lateGhostDeleteSessionRecord
	deleted := make(chan struct{})
	lateGhostDeleteSessionRecord = func(*Manager, string, string, string, error) (bool, error) {
		close(deleted)
		return true, nil
	}
	t.Cleanup(func() { lateGhostDeleteSessionRecord = previousDelete })

	subscriberID, events := manager.events.subscribe()
	t.Cleanup(func() { manager.events.unsubscribe(subscriberID) })
	lateResult := make(chan error, 1)
	lateResult <- nil
	manager.reconcileLateGhostCleanup(repoID, session.RootSessionTitle, key, stableID, lateResult)

	select {
	case <-deleted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("late ghost finalizer did not consume its durable row")
	}
	manager.mu.Lock()
	_, rootGraceArmed := manager.rootKilledAt[repoID]
	manager.mu.Unlock()
	assert.True(t, rootGraceArmed, "late root cleanup must arm the same grace window as synchronous kill")

	select {
	case event := <-events:
		assert.Equal(t, agentproto.EventSessionKilled, event.Type)
		var data session.InstanceData
		require.NoError(t, json.Unmarshal(event.Data, &data))
		assert.Equal(t, stableID, data.ID)
		assert.Equal(t, session.RootSessionTitle, data.Title)
	case <-time.After(250 * time.Millisecond):
		t.Error("late ghost cleanup removed the row without publishing session.killed")
	}
}

func TestKillSession_LiveCleanupSettlesDurablyBeforeTailFailure(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "live-tail")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "live-tail", RepoID: repoID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "origin repo "+repoPath+" is gone")
	recovery := recordFor(t, repoID, "live-tail").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.State)

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
