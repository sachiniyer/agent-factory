package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// registerGoneWorktree registers a live, Ready session with a real linked
// worktree and then deletes that worktree outside af — the #5102 shape: `rm -rf`
// of the directory, with git's registration and the branch left behind.
func registerGoneWorktree(t *testing.T, m *Manager, repoID, repoPath, title string) (*session.Instance, string) {
	t.Helper()
	inst, wtPath := registerArchivable(t, m, repoID, repoPath, title)
	require.NoError(t, os.RemoveAll(wtPath))
	return inst, wtPath
}

func requireBranchExists(t *testing.T, repoPath, branch string) {
	t.Helper()
	out, err := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput()
	require.NoError(t, err, "branch %s must be kept: %s", branch, out)
}

// The poll flags a live row whose worktree vanished, persists the flag, and the
// projection every listing surface reads carries it (#5102).
func TestRefreshStatuses_FlagsVanishedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")

	manager.RefreshStatuses()

	missing, reason := inst.WorktreeMissing()
	require.True(t, missing)
	assert.Contains(t, reason, wtPath)
	rec := recordFor(t, repoID, "vanished")
	require.NotNil(t, rec)
	assert.True(t, rec.Worktree.Missing, "the flag must be durable so a restart keeps it")
	assert.True(t, inst.ToInstanceData().Worktree.Missing)
}

// Archived rows are probed too: an archive directory deleted outside af is a
// fact about the row, and the probe fan-out must reach unstarted rows.
func TestRefreshStatuses_FlagsVanishedArchiveDirectory(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "archived-then-deleted")
	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "archived-then-deleted", RepoID: repoID})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(archivedPath))

	manager.RefreshStatuses()

	missing, reason := inst.WorktreeMissing()
	require.True(t, missing)
	assert.Contains(t, reason, archivedPath)
}

// Archive of a session whose worktree was deleted outside af used to refuse
// forever with "cannot claim worktree path …: no such file or directory". It must
// now succeed: tmux torn down, row Archived and flagged, path unchanged, branch
// kept (#5102).
func TestArchiveSession_VanishedWorktreeArchivesKeepingBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")
	branch := inst.GetBranch()
	if branch == "" {
		branch = "af/vanished"
	}

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, wtPath, archivedPath, "nothing moved, so the recorded path is the one reported")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.True(t, data.Worktree.Missing)
	assert.Equal(t, wtPath, data.Worktree.WorktreePath)
	assert.False(t, inst.Started(), "tmux must be torn down")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the archive fence must be released")
	requireBranchExists(t, repoPath, branch)

	rec := recordFor(t, repoID, "vanished")
	require.NotNil(t, rec)
	assert.Equal(t, session.Archived, rec.Status)
	assert.True(t, rec.Worktree.Missing)
	assert.Equal(t, wtPath, rec.Worktree.WorktreePath)
	_, statErr := os.Lstat(wtPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the gone route must not create anything")
}

// An archive whose move landed but was never recorded (daemon died between the
// move and the persist) leaves the record naming the vacated source with no
// recovery record — a bare ENOENT, exactly like an outside deletion. The retry
// must adopt the bytes git registers at the archive destination, not commit a
// row pointing at nothing (#5102).
func TestArchiveSession_AdoptsALandedEarlierArchiveMove(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "landed")
	dest, err := archivedWorktreePath(repoID, "landed")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", wtPath, dest).CombinedOutput()
	require.NoError(t, err, string(out))

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "landed", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, dest, archivedPath)
	assert.Equal(t, dest, data.Worktree.WorktreePath)
	assert.False(t, data.Worktree.Missing, "an adopted move is a complete archive, not a missing worktree")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.False(t, inst.Started())
	dirty, err := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, err, "the landed bytes must be kept where they are")
	assert.Equal(t, "uncommitted", string(dirty))
	requireBranchExists(t, repoPath, "af/landed")
	rec := recordFor(t, repoID, "landed")
	require.NotNil(t, rec)
	assert.Equal(t, dest, rec.Worktree.WorktreePath)
}

// A destination occupied by something git does not prove is this session's
// worktree is neither a deletion nor a landed move af can vouch for: refuse,
// name both paths, and leave the session and the occupant untouched.
func TestArchiveSession_RefusesAnUnprovenOccupiedDestination(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "occupied")
	dest, err := archivedWorktreePath(repoID, "occupied")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dest, 0o755))
	marker := filepath.Join(dest, "keep.txt")
	require.NoError(t, os.WriteFile(marker, []byte("not af's"), 0o644))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "occupied", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), wtPath)
	assert.Contains(t, err.Error(), dest)

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(), "a refusal before teardown leaves the session live")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the archive fence must be lowered")
	assert.Equal(t, wtPath, inst.GetWorktreePath(), "nothing was adopted")
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "not af's", string(data))
}

// Restoring a row archived with its worktree already gone keeps the pre-#5102
// behavior in this slice: the relocation claim finds nothing to move back and
// restore refuses, leaving the row archived, flagged, and its branch intact.
// Rebuilding it from the branch is deferred to #5150.
func TestRestoreArchived_VanishedWorktreeStillRefuses(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "vanished", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), wtPath)
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing, "a refused restore must not clear the flag")
	requireBranchExists(t, repoPath, "af/vanished")
}
