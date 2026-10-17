package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A deleted worktree that reappears during the gone route's teardown holds an
// unverified directory. The archive fails as unknown and the record is fenced,
// so the drop to Lost cannot hand that directory to automatic recovery.
func TestArchiveSession_GoneWorktreeReappearingDuringTeardownIsFenced(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "reappears")

	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.False(t, adopted, "premise: this archive took the deletion route")
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		return prev(i, before, trust, adopted)
	}
	t.Cleanup(func() { archiveGoneTeardown = prev })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "reappears", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	_, _, unresolved := inst.GetWorktreeRelocationCandidates()
	assert.True(t, unresolved, "Lost recovery must refuse the reappeared directory")
	rec := recordFor(t, repoID, "reappears")
	require.NotNil(t, rec)
	assert.NotNil(t, rec.Worktree.RelocationRecovery, "the fence must be durable")
}

// On the deletion route the on-archive hook still runs — in a scratch
// directory, not the deleted path (which cannot be a cwd) and not the repo root
// (where a prune of "." would hit the user's main checkout).
func TestArchiveSession_DeletionRouteRunsHookInScratchDir(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "hook-gone")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf(
		`test -z "$AF_ARCHIVE_PATH" && test "$AF_WORKTREE_PATH" = %q && `+
			`test "$PWD" != %q && test -z "$(ls -A)" && printf ran > %q`,
		wtPath, repoPath, marker))

	_, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "hook-gone", RepoID: repoID})
	require.NoError(t, err, "the hook must run cleanly, so no hook-failure warning either")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	got, err := os.ReadFile(marker)
	require.NoError(t, err, "the hook must have run")
	assert.Equal(t, "ran", string(got))
}

// fenceGoneArchive drives the reappearance fence: the deleted worktree's path is
// recreated during the gone route's teardown, so the archive fails fenced and the
// row drops to Lost with an identity-unknown stall on record.
func fenceGoneArchive(t *testing.T, manager *Manager, repoID, title, wtPath string) {
	t.Helper()
	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		return prev(i, before, trust, adopted)
	}
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	archiveGoneTeardown = prev
	require.Error(t, err)
	require.Contains(t, err.Error(), "fenced")
}

// The fence must not outlive what it guards: once the path is conclusively
// absent again, a retried archive takes the deletion route (#5102).
func TestArchiveSession_FencedRowArchivesOnceThePathIsGoneAgain(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "refenced")
	fenceGoneArchive(t, manager, repoID, "refenced", wtPath)

	// While the path exists it stays unverified: the gone route stamped the
	// flag before teardown, so the retry refuses instead of moving it.
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing, "the gone route records the absence it proved")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "refenced", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
	assert.DirExists(t, wtPath, "the unverified directory must not be moved into the archive")

	require.NoError(t, os.Remove(wtPath))
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "refenced", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, session.LiveArchived, archived.Liveness)
	assert.True(t, archived.Worktree.Missing)
	assert.Nil(t, archived.Worktree.RelocationRecovery, "the discharged fence must not be persisted again")
}

// Likewise kill: the identity-unknown fence must not make the row unkillable
// once its path is gone, and must keep refusing while it is not.
func TestKillSession_FencedRowKillableOnceThePathIsGoneAgain(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "refenced-kill")
	fenceGoneArchive(t, manager, repoID, "refenced-kill", wtPath)
	marker := filepath.Join(wtPath, "unverified.txt")
	require.NoError(t, os.WriteFile(marker, []byte("x"), 0o644))

	_, err := manager.KillSession(KillSessionRequest{Title: "refenced-kill", RepoID: repoID})
	require.Error(t, err, "the fence must hold while the unverified path exists")
	assert.False(t, inst.UserKilled())
	assert.FileExists(t, marker, "kill must not delete the unverified directory")

	require.NoError(t, os.RemoveAll(wtPath))
	_, err = manager.KillSession(KillSessionRequest{Title: "refenced-kill", RepoID: repoID})
	require.NoError(t, err)
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "refenced-kill")]
	manager.mu.Unlock()
	assert.False(t, tracked, "a completed kill removes the row")
}

// plantStuckEditor makes the next VS Code stop for key fail to confirm exit.
func plantStuckEditor(t *testing.T, manager *Manager, inst *session.Instance, key, worktree string) {
	t.Helper()
	cmd, _ := startOwnedSleep(t)
	manager.vscode.mu.Lock()
	manager.vscode.servers[key] = &vscodeServer{
		worktree: worktree, instanceID: inst.ID, cmd: cmd, exited: make(chan struct{}),
		stopGrace: 10 * time.Millisecond,
		killGroup: func(int, syscall.Signal) error { return nil },
	}
	manager.vscode.mu.Unlock()
}

// Once a landed earlier move is adopted, the record names a directory git
// proved. A worktree-missing flag the poll set against the vacated source must
// not be persisted beside it — including by a cancel path before teardown — or
// the sticky-flag guard would refuse this verified worktree forever (#5102).
func TestArchiveSession_AdoptedRouteCancelPersistsNoStaleMissingFlag(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "adopt-cancel")
	dest, err := archivedWorktreePath(repoID, "adopt-cancel")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)
	manager.RefreshStatuses()
	missing, _ := inst.WorktreeMissing()
	require.True(t, missing, "premise: the poll flagged the vacated source")

	key := daemonInstanceKey(repoID, "adopt-cancel")
	plantStuckEditor(t, manager, inst, key, dest)
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-cancel", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VS Code editor teardown could not be confirmed")
	rec := recordFor(t, repoID, "adopt-cancel")
	require.NotNil(t, rec)
	assert.Equal(t, dest, rec.Worktree.WorktreePath, "the cancel persisted the adopted path")
	assert.False(t, rec.Worktree.Missing, "and must not have persisted the stale flag beside it")

	manager.vscode.mu.Lock()
	delete(manager.vscode.servers, key)
	manager.vscode.mu.Unlock()
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-cancel", RepoID: repoID})
	require.NoError(t, err, "the adopted worktree must stay archivable")
	assert.Equal(t, session.LiveArchived, archived.Liveness)
	assert.Equal(t, dest, archived.Worktree.WorktreePath)
}

// If the adopted directory vanishes during teardown, the disappearance is
// recorded: the row is fenced and flagged, so a directory created at that path
// later is refused by archive (and by respawn) instead of being accepted (#5102).
func TestArchiveSession_AdoptedDirectoryVanishingIsRecordedAsGone(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "adopt-vanish")
	dest, err := archivedWorktreePath(repoID, "adopt-vanish")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.True(t, adopted, "premise: this archive took the adopted route")
		require.NoError(t, os.Rename(dest, dest+".gone"))
		return prev(i, before, trust, adopted)
	}
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-vanish", RepoID: repoID})
	archiveGoneTeardown = prev
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced")
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing, "the vanished adopted directory must be recorded as gone")
	rec := recordFor(t, repoID, "adopt-vanish")
	require.NotNil(t, rec)
	assert.True(t, rec.Worktree.Missing)
	assert.NotNil(t, rec.Worktree.RelocationRecovery)

	// A stranger recreated at the path is not accepted.
	require.NoError(t, os.Mkdir(dest, 0o755))
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "adopt-vanish", RepoID: repoID})
	require.Error(t, err)
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.DirExists(t, dest)
}

// A flag transition whose write cedes to an operation that started after the
// probe must not be persisted mid-operation, and must not be forgotten: the
// next pass writes it once the operation is gone (#5102).
func TestRefreshWorktreeMissing_CededWriteIsRetried(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "ceded")

	require.NoError(t, inst.Transition(session.BeginArchive()))
	_, changed := inst.RefreshWorktreeMissing()
	require.False(t, changed, "premise: the probe itself refuses to stamp under an op")
	require.NoError(t, inst.Transition(session.CancelArchive()))

	// Stamp it with no op, then raise one before the write, as a racing
	// operation would.
	_, changed = inst.RefreshWorktreeMissing()
	require.True(t, changed)
	require.NoError(t, inst.Transition(session.BeginArchive()))
	key := stableSessionKey(repoID, inst)
	if !manager.persistWorktreeMissing(repoID, inst) {
		manager.markWorktreeMissingUndurable(key)
	}
	rec := recordFor(t, repoID, "ceded")
	require.NotNil(t, rec)
	assert.False(t, rec.Worktree.Missing, "a write that cedes to an op must not land")

	require.NoError(t, inst.Transition(session.CancelArchive()))
	manager.refreshWorktreeMissing(repoID, inst)
	rec = recordFor(t, repoID, "ceded")
	require.NotNil(t, rec)
	assert.True(t, rec.Worktree.Missing, "the ceded transition is retried and lands")
}

// A row archived through the no-move deletion route keeps its vanished LIVE
// path. That basename is not an archive directory, so it must not be claimed —
// an unrelated session named after it would be refused forever — while the
// title-derived claim stays (#5102).
func TestArchiveClaimName_MissingRowDoesNotClaimItsLivePathBasename(t *testing.T) {
	data := session.InstanceData{Title: "feature", Liveness: session.LiveArchived}
	data.Worktree.WorktreePath = filepath.Join("/parent", "repo-foo")
	data.Worktree.Missing = true
	assert.False(t, archiveRecordClaimsTitle(data, "repo-foo"))
	assert.True(t, archiveRecordClaimsTitle(data, "feature"))

	data.Worktree.Missing = false
	assert.True(t, archiveRecordClaimsTitle(data, "repo-foo"),
		"control: an unflagged archived row still claims its recorded directory")
}
