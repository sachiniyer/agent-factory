package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archivedInstanceWithRecoveryClaim(
	t *testing.T, title string,
) (*Manager, string, string, *session.Instance, string) {
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
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)
	assert.True(t, recovery.IdentityKnown)

	inst.SetBackend(&session.LocalBackend{})
	_, err = manager.KillSession(KillSessionRequest{
		Title: "repo-gone-authorization", RepoID: repoID,
	})
	require.NoError(t, err, "slice 2 must consume cleanup-ready through the live descriptor transaction")
	assert.True(t, inst.UserKilled(), "successful cleanup must commit the user kill")
	assert.False(t, exists(archivedPath), "successful cleanup must remove the exact authorized archive")
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
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)
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
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_OriginProbeFailurePersistsUnresolvedClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "probe-unknown")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "probe-unknown", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "probe-unknown").Worktree.RelocationRecovery)
	t.Setenv("PATH", t.TempDir())

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "probe-unknown", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "unknown origin probe must leave the archive intact")
	recovery := recordFor(t, repoID, "probe-unknown").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "record-free claim must become a durable non-destructive fence")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_PathDerivationFailurePersistsUnresolvedClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "path-unknown")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "path-unknown", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "path-unknown").Worktree.RelocationRecovery)

	previous := beforeRestoreWorktreePath
	beforeRestoreWorktreePath = func() {
		require.NoError(t, os.RemoveAll(repoPath), "remove origin after the guard but before destination derivation")
	}
	t.Cleanup(func() { beforeRestoreWorktreePath = previous })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "path-unknown", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "path derivation failure must leave the archive intact")
	recovery := recordFor(t, repoID, "path-unknown").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "path derivation failure must materialize a durable unresolved claim")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_AuthoritativeProbeFailurePersistsUnresolvedClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "use-probe-unknown")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "use-probe-unknown", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "use-probe-unknown").Worktree.RelocationRecovery)

	previous := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() { t.Setenv("PATH", t.TempDir()) }
	t.Cleanup(func() { beforeRestoreWorktreeUse = previous })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "use-probe-unknown", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "authoritative probe failure must leave the archive intact")
	recovery := recordFor(t, repoID, "use-probe-unknown").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "authoritative probe failure must materialize a durable unresolved claim")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_GenericUseFailurePersistsRecordFreeClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "use-failure")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "use-failure", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, "use-failure").Worktree.RelocationRecovery)
	destination, err := sessiongit.RestoreWorktreePath(repoPath, "use-failure", inst.GetBranch())
	require.NoError(t, err)

	previous := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		require.NoError(t, os.Mkdir(destination, 0o755),
			"occupy the selected destination after path derivation")
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = previous })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "use-failure", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, exists(archivedPath), "generic restore failure must leave the archive intact")
	recovery := recordFor(t, repoID, "use-failure").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "generic restore failure must persist its record-free claim")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.Empty(t, recovery.CleanupLifecycle)
}

func TestRestoreArchived_CleanupPreparationPersistFailureLeavesStaleFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "prepare-persist-failure")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "prepare-persist-failure", RepoID: repoID})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(repoPath))

	diskFull := errors.New("forced cleanup-ready persistence failure")
	previousPersist := testHookPersistInstanceData
	writes := 0
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != "prepare-persist-failure" {
			return nil
		}
		writes++
		if writes == 2 {
			return diskFull
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = previousPersist })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "prepare-persist-failure", RepoID: repoID})
	require.Error(t, err)
	assert.ErrorContains(t, err, diskFull.Error())
	assert.Equal(t, 2, writes, "cleanup preparation must stage a durable stale fence before cleanup_ready")
	recovery := recordFor(t, repoID, "prepare-persist-failure").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "failed cleanup-ready persistence must leave the staged durable fence")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
}

func TestRestoreArchived_AuthoritativeRepoGonePersistFailureLeavesStaleFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "late-gone-persist-failure")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "late-gone-persist-failure", RepoID: repoID})
	require.NoError(t, err)

	previousUse := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		require.NoError(t, os.RemoveAll(repoPath), "remove origin immediately before the authoritative probe")
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = previousUse })

	diskFull := errors.New("forced authoritative cleanup-ready persistence failure")
	previousPersist := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != "late-gone-persist-failure" || data.Worktree.RelocationRecovery == nil {
			return nil
		}
		if data.Worktree.RelocationRecovery.CleanupLifecycle == sessiongit.RelocationRecoveryCleanupReady {
			return diskFull
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = previousPersist })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "late-gone-persist-failure", RepoID: repoID})
	require.Error(t, err)
	assert.ErrorContains(t, err, diskFull.Error())
	recovery := recordFor(t, repoID, "late-gone-persist-failure").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "failed late cleanup-ready persistence must leave a staged durable fence")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.True(t, recovery.IdentityKnown)
}

func TestRestoreArchived_AuthoritativeGenerationFailurePersistsStaleFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "late-generation-failure")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "late-generation-failure", RepoID: repoID})
	require.NoError(t, err)

	previousUse := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		require.NoError(t, os.RemoveAll(repoPath), "remove origin immediately before the authoritative probe")
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = previousUse })
	generationFailure := errors.New("forced cleanup generation failure")
	restoreInstall := sessiongit.SetCleanupGenerationInstallErrorForTest(generationFailure)
	t.Cleanup(restoreInstall)

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "late-generation-failure", RepoID: repoID})
	require.Error(t, err)
	assert.ErrorContains(t, err, generationFailure.Error())
	recovery := recordFor(t, repoID, "late-generation-failure").Worktree.RelocationRecovery
	require.NotNil(t, recovery, "late generation failure must leave its pre-authorization fence durable")
	assert.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	assert.Empty(t, recovery.CleanupLifecycle)
}

func TestRestoreArchived_RepoReturnsBeforeKillRefusesBeforeTombstone(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := archivedInstanceWithRecoveryClaim(t, "repo-returned")
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "repo-returned", RepoID: repoID})
	require.Error(t, err)
	recovery := recordFor(t, repoID, "repo-returned").Worktree.RelocationRecovery
	require.NotNil(t, recovery)
	require.Equal(t, sessiongit.RelocationRecoveryClaimStale, recovery.State)
	require.Equal(t, sessiongit.RelocationRecoveryCleanupReady, recovery.CleanupLifecycle)
	require.NoError(t, exec.Command("git", "init", repoPath).Run(),
		"bring a valid origin repository back before kill")

	_, err = manager.KillSession(KillSessionRequest{Title: "repo-returned", RepoID: repoID})
	require.Error(t, err)
	assert.False(t, inst.UserKilled(), "the generic guard must run before the kill tombstone")
	assert.True(t, exists(archivedPath), "a refused kill must leave the archive intact")
	assert.NotNil(t, recordFor(t, repoID, "repo-returned").Worktree.RelocationRecovery)
}
