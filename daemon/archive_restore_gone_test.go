package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

func requireWorktreeOnBranch(t *testing.T, path, branch string) {
	t.Helper()
	out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, branch, strings.TrimSpace(string(out)))
}

// A session archived through the deletion route is restorable: there are no
// bytes to move back, so restore rebuilds the worktree from the kept branch at
// the destination the moving route would use, clears the flag, and commits the
// live row (#5102). It used to refuse at the relocation claim forever.
func TestRestoreArchived_VanishedWorktreeRebuildsFromBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)
	require.True(t, archived.Worktree.Missing)

	restoredPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.Equal(t, restoredPath, inst.GetWorktreePath())
	requireWorktreeOnBranch(t, restoredPath, "af/vanished")
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing, "af re-placed the worktree, so the flag clears")
	rec := recordFor(t, repoID, "vanished")
	require.NotNil(t, rec)
	assert.False(t, rec.Worktree.Missing)
	assert.Equal(t, restoredPath, rec.Worktree.WorktreePath)
	assert.NotEqual(t, session.Archived, rec.Status)
}

// An archived row whose archive directory was deleted after the archive (no
// flag yet: the poll never ran) takes the same rebuild route.
func TestRestoreArchived_DeletedArchiveDirectoryRebuildsFromBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "deleted-archive", "deleted-archive")
	branch := inst.GetBranch()
	require.NoError(t, os.RemoveAll(inst.GetWorktreePath()))

	restoredPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "deleted-archive", RepoID: repoID})
	require.NoError(t, err)
	requireWorktreeOnBranch(t, restoredPath, branch)
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
}

// A flagged archived row with a directory recreated at its recorded path must
// not adopt it: restore refuses, the stranger's files are untouched, the flag
// stays — and once the path is cleared, restore rebuilds from the branch.
func TestRestoreArchived_FlaggedRowWithARecreatedDirectoryRefuses(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "recreated")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "recreated", RepoID: repoID})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wtPath, 0o755))
	marker := filepath.Join(wtPath, "stranger.txt")
	require.NoError(t, os.WriteFile(marker, []byte("not the session's"), 0o644))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "recreated", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
	assert.Contains(t, err.Error(), wtPath)
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.Equal(t, wtPath, inst.GetWorktreePath())
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing)
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr)
	assert.Equal(t, "not the session's", string(data))

	require.NoError(t, os.RemoveAll(wtPath))
	restoredPath, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "recreated", RepoID: repoID})
	require.NoError(t, err)
	requireWorktreeOnBranch(t, restoredPath, "af/recreated")
}

// A user's own `git worktree move` of the archive leaves the recorded path
// absent while the bytes are intact elsewhere. Restore must not rebuild a
// second checkout; it refuses and names where git has the branch.
func TestRestoreArchived_RefusesAnArchiveTheUserMoved(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "user-moved-archive", "user-moved-archive")
	archivedPath := inst.GetWorktreePath()
	// Resolved, because git reports the real path and macOS's temp dir is a
	// symlink.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	elsewhere := filepath.Join(base, "mine")
	moveWorktreeForTest(t, repoPath, archivedPath, elsewhere)

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "user-moved-archive", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), elsewhere)
	assert.Contains(t, err.Error(), archivedPath)
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.Equal(t, archivedPath, inst.GetWorktreePath())
	dirty, readErr := os.ReadFile(filepath.Join(elsewhere, "dirty.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "uncommitted-user-moved-archive", string(dirty))
}

// A rebuild that fails (here: the kept branch was deleted after the archive)
// leaves the archived row as it was — archived, flagged, at its recorded path —
// and persisted that way.
func TestRestoreArchived_FailedRebuildLeavesTheArchivedRow(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "branchless")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "branchless", RepoID: repoID})
	require.NoError(t, err)
	out, gitErr := exec.Command("git", "-C", repoPath, "worktree", "prune").CombinedOutput()
	require.NoError(t, gitErr, string(out))
	out, gitErr = exec.Command("git", "-C", repoPath, "branch", "-D", "af/branchless").CombinedOutput()
	require.NoError(t, gitErr, string(out))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "branchless", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the session stays archived")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.Equal(t, wtPath, inst.GetWorktreePath(), "the record is re-aimed back at its recorded path")
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing)
	rec := recordFor(t, repoID, "branchless")
	require.NotNil(t, rec)
	assert.Equal(t, session.Archived, rec.Status)
	assert.Equal(t, wtPath, rec.Worktree.WorktreePath)
}
