package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// moveWorktreeForTest runs `git worktree move`, creating dest's parent first.
func moveWorktreeForTest(t *testing.T, repoPath, src, dest string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", src, dest).CombinedOutput()
	require.NoError(t, err, string(out))
}

// A user's own `git worktree move` elsewhere leaves source and archive
// destination both absent — the deletion footprint — while the worktree is
// intact where git now registers it. Archiving it as missing would strand it;
// refuse and name where it went (#5102).
func TestArchiveSession_RefusesAWorktreeTheUserMoved(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "user-moved")
	elsewhere := filepath.Join(t.TempDir(), "my-moved-worktree")
	moveWorktreeForTest(t, repoPath, wtPath, elsewhere)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "user-moved", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "moved outside af")
	assert.Contains(t, err.Error(), filepath.Base(elsewhere))

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing, "a refused archive must not stamp the row missing")
	dirty, err := os.ReadFile(filepath.Join(elsewhere, "dirty.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted", string(dirty))
}

// A flag set by a probe that raced af's own move is stale the moment af commits
// a row whose worktree it placed: an ordinary archive must not carry it, or the
// row refuses prompts forever after a later restore (#5102).
func TestArchiveSession_MovedArchiveClearsAStaleMissingFlag(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "stale-flag")
	inst.SetWorktreeMissing("tracked worktree path /x does not exist (deleted outside af)")

	_, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "stale-flag", RepoID: repoID})
	require.NoError(t, err)
	assert.False(t, data.Worktree.Missing)
	rec := recordFor(t, repoID, "stale-flag")
	require.NotNil(t, rec)
	assert.False(t, rec.Worktree.Missing, "the committed record must not carry the stale flag")
}

// The restore-side twin: a moved-back worktree is one af placed, so a flag on
// the archived row does not survive the restore.
func TestRestoreArchived_MovedRestoreClearsAStaleMissingFlag(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "stale-restore", "stale-restore")
	inst.SetWorktreeMissing("tracked worktree path /x does not exist (deleted outside af)")

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "stale-restore", RepoID: repoID})
	require.NoError(t, err)
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing)
}

// On the adopted route the bytes are already in the archive, so the on-archive
// hook's AF_ARCHIVE_PATH names that location, as the relocating route's does.
func TestArchiveSession_AdoptedRoutePassesArchivePathToHook(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, wtPath := registerArchivable(t, manager, repoID, repoPath, "hooked")
	dest, err := archivedWorktreePath(repoID, "hooked")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf(
		`test "$AF_ARCHIVE_PATH" = %q && test "$AF_WORKTREE_PATH" = %q && printf ran > %q`,
		dest, dest, marker))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "hooked", RepoID: repoID})
	require.NoError(t, err)
	got, err := os.ReadFile(marker)
	require.NoError(t, err, "the hook must have run with AF_ARCHIVE_PATH set to the adopted archive path")
	assert.Equal(t, "ran", string(got))
}

// A directory swapped in under the adopted name during teardown is caught by
// device/inode, not passed by a fresh lstat: the archive fails, the record is
// fenced, and the real bytes are untouched (#5102).
func TestArchiveSession_AdoptedDirectorySwappedDuringTeardownIsRefused(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "swapped")
	dest, err := archivedWorktreePath(repoID, "swapped")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)
	realDir := dest + ".real"

	prev := archiveGoneTeardown
	archiveGoneTeardown = func(i *session.Instance, before func() error, trust, adopted bool) (error, error) {
		require.True(t, adopted, "premise: this archive took the adopted route")
		require.NoError(t, os.Rename(dest, realDir))
		require.NoError(t, os.Mkdir(dest, 0o755))
		return prev(i, before, trust, adopted)
	}
	t.Cleanup(func() { archiveGoneTeardown = prev })

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "swapped", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	_, _, unresolved := inst.GetWorktreeRelocationCandidates()
	assert.True(t, unresolved, "the record must be fenced against respawn and cleanup")
	dirty, err := os.ReadFile(filepath.Join(realDir, "dirty.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted", string(dirty))
}
