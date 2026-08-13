package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

func preparedCleanupRecovery(
	t *testing.T, repoPath, archive, title string,
) sessiongit.RelocationRecovery {
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
	ghostCleanupWorktree = func(
		data *session.InstanceData,
		_ string,
		_ func(*session.InstanceData) error,
	) (sessiongit.CleanupState, error, <-chan error) {
		cleanupCalls++
		current := data.RestoreArchiveRollbackFence()
		current, err := current.RestoreRelocationRecoveryOriginals()
		require.NoError(t, err)
		current.Worktree.RelocationRecovery.State = sessiongit.RelocationRecoveryCleanupStalled
		*data = current
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

func TestLateGhostCleanup_WorkerErrorReleasesProcessFence(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-late-error")
	key := daemonInstanceKey(repoID, "ghost-late-error")
	stableID := "ghost-late-error-id"
	manager.markGhostCleanupStalled(key, stableID)

	lateResult := make(chan error, 1)
	lateResult <- errors.New("transient descriptor failure")
	manager.reconcileLateGhostCleanup(repoID, "ghost-late-error", key, stableID, lateResult)

	require.Eventually(t, func() bool {
		return !manager.ghostCleanupStallActive(key, stableID)
	}, 250*time.Millisecond, time.Millisecond, "a completed worker error must release the process-only fence")
	assert.NotNil(t, recordFor(t, repoID, "ghost-late-error"),
		"releasing the process fence must retain the durable cleanup row for retry")
}

func TestKillSession_GhostCleanupSettledTailFailureRetriesFinalization(t *testing.T) {
	manager, repoID := newCleanupReadyGhost(t, "ghost-sync-success")
	stubGhostTmux(t, tmux.PaneStateKnown, nil)
	retained := filepath.Join(t.TempDir(), ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(retained, 0o755))
	record := recordFor(t, repoID, "ghost-sync-success")
	record.ArchiveReport = retainedTreeReport(t, retained)
	require.NoError(t, persistInstanceData(repoID, *record))
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale,
		recordFor(t, repoID, "ghost-sync-success").Worktree.RelocationRecovery.State)

	previousCleanup := ghostCleanupWorktree
	var releaseLock func()
	ghostCleanupWorktree = func(
		data *session.InstanceData,
		_ string,
		_ func(*session.InstanceData) error,
	) (sessiongit.CleanupState, error, <-chan error) {
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

	_, err := manager.KillSession(KillSessionRequest{Title: "ghost-sync-success", RepoID: repoID})
	require.ErrorIs(t, err, config.ErrLockTimeout)
	require.NotNil(t, releaseLock, "the tail failure must happen after descriptor cleanup succeeded")
	releaseLock()
	releaseLock = nil

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
	record := recordFor(t, repoID, "ghost-crash-window")
	record.ArchiveReport = retainedTreeReport(t, retained)
	require.NoError(t, persistInstanceData(repoID, *record))
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale,
		recordFor(t, repoID, "ghost-crash-window").Worktree.RelocationRecovery.State)

	previousCleanup := ghostCleanupWorktree
	var releaseLock func()
	ghostCleanupWorktree = func(
		data *session.InstanceData,
		title string,
		checkpoint func(*session.InstanceData) error,
	) (sessiongit.CleanupState, error, <-chan error) {
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

	_, err := manager.KillSession(KillSessionRequest{Title: "ghost-crash-window", RepoID: repoID})
	require.ErrorIs(t, err, config.ErrLockTimeout)
	require.NotNil(t, releaseLock, "the tail failure must happen after descriptor cleanup succeeded")
	record = recordFor(t, repoID, "ghost-crash-window")
	require.NotNil(t, record)
	require.NotNil(t, record.Worktree.RelocationRecovery)
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, record.Worktree.RelocationRecovery.State)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupFinalizing,
		record.Worktree.RelocationRecovery.CleanupLifecycle,
		"the durable row must record that descriptor cleanup entered its finalization fence")
	assert.True(t, record.ArchiveReport == nil || record.ArchiveReport.Empty(),
		"the same checkpoint must retire the consumed retained-tree handle")

	releaseLock()
	releaseLock = nil
	close(releaseOldFinalizer)
	oldFinalizerReleased = true
}

func retainedTreeReport(t *testing.T, retained string) *sessiongit.ArchiveReport {
	t.Helper()
	info, err := os.Stat(retained)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: retained, IdentityKnown: true,
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino),
		FileType: uint32(stat.Mode & syscall.S_IFMT),
	}}}
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
	assert.Equal(t, originalBranch, snapshot.Worktree.BranchName,
		"a late checkpoint must not read mutable caller data")
	completed := <-result
	require.Error(t, completed.err)
	assert.Equal(t, sessiongit.CleanupStateUnknown, completed.state)
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
		ArchiveReport: retainedProjectionFixture(root),
	}
	stored := data.ForStorage()
	require.NotNil(t, stored.Worktree.RelocationRecovery)
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale, stored.Worktree.RelocationRecovery.State)
	require.Empty(t, stored.Worktree.RelocationRecovery.CleanupLifecycle,
		"the outer archive fence uses its own inert claim instead of exposing the cleanup lifecycle")
	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence)
	require.NotNil(t, stored.ArchiveReport.RollbackFence.OriginalRelocationRecovery)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady,
		stored.ArchiveReport.RollbackFence.OriginalRelocationRecovery.State,
		"the archive rollback fence must retain the cleanup authorization current admission restores")

	require.NoError(t, validateGhostWorktreeDestructionAdmission(&stored),
		"current admission must decode cleanup_ready before interpreting the projected state")
}

func retainedProjectionFixture(root string) *sessiongit.ArchiveReport {
	return &sessiongit.ArchiveReport{RetainedTrees: []sessiongit.ArchiveRetainedTree{{
		Path: filepath.Join(root, "retained-source"), IdentityKnown: true,
		Device: 17, Inode: 23, FileType: uint32(syscall.S_IFDIR),
		Skipped: []sessiongit.ArchiveSkippedEntry{{
			Path: "private/work", Reason: sessiongit.ArchiveSkipPermissionDenied,
		}},
	}}}
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

	// Read rootKilledAt only AFTER the event receive (#3283). `deleted` closes
	// inside the finalizer's record-delete call, BEFORE completeLateGhostKill
	// arms the grace window, so a read anchored on it races the arm. The event
	// is the correct happens-before anchor: completeLateGhostKill arms root
	// grace before publishing removal — the product invariant its own comment
	// states — so by the time the event is received the map write is ordered
	// before this read.
	manager.mu.Lock()
	_, rootGraceArmed := manager.rootKilledAt[repoID]
	manager.mu.Unlock()
	assert.True(t, rootGraceArmed, "late root cleanup must arm the same grace window as synchronous kill")
}
