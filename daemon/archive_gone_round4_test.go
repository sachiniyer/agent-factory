package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// A restore that adopts a landed earlier restore move must re-confirm the
// proven directory by device/inode before the respawn launches an agent in it.
// A swap after the adoption is refused, the record fenced, and the real bytes
// left untouched (#5102).
func TestRestoreArchived_AdoptedDirectorySwappedBeforeRespawnIsRefused(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "swap-restore", "swap-restore")
	archivedPath := inst.GetWorktreePath()
	restoreDest, err := sessiongit.RestoreWorktreePath(repoPath, "swap-restore", inst.GetBranch())
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, archivedPath, restoreDest)
	realDir := restoreDest + ".real"

	prev := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		// Runs after the adoption proof, before the respawn.
		require.Equal(t, restoreDest, inst.GetWorktreePath(), "premise: the restore adopted the landed move")
		require.NoError(t, os.Rename(restoreDest, realDir))
		require.NoError(t, os.Mkdir(restoreDest, 0o755))
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = prev })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "swap-restore", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(), "no agent may have been started")
	_, _, unresolved := inst.GetWorktreeRelocationCandidates()
	assert.True(t, unresolved)
	rec := recordFor(t, repoID, "swap-restore")
	require.NotNil(t, rec)
	assert.NotNil(t, rec.Worktree.RelocationRecovery, "the fence must be durable")
	dirty, err := os.ReadFile(filepath.Join(realDir, "dirty.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted-swap-restore", string(dirty))
}

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
