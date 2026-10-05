package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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
func TestRefreshInstanceStatus_FlagsVanishedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")

	manager.refreshInstanceStatus(repoID, inst)

	missing, reason := inst.WorktreeMissing()
	require.True(t, missing)
	assert.Contains(t, reason, wtPath)
	rec := recordFor(t, repoID, "vanished")
	require.NotNil(t, rec)
	assert.True(t, rec.Worktree.Missing, "the flag must be durable so a restart keeps it")
	assert.True(t, inst.ToInstanceData().Worktree.Missing)
}

// send-prompt into a deleted cwd is refused before anything is sent, with the
// two commands that resolve it, and stays refundable (notAttempted).
func TestSendPromptWithStatus_RefusesVanishedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")
	rec := &promptRecorder{}
	inst.SetBackend(recordingBackend{readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()}, rec: rec})

	status, err := manager.SendPromptWithStatus(SendPromptRequest{Title: "vanished", RepoID: repoID, Prompt: "ship it"})
	require.Error(t, err)
	assert.Equal(t, session.PromptCouldNotConfirm, status)
	assert.Contains(t, err.Error(), "af sessions archive")
	assert.Contains(t, err.Error(), "af sessions kill")
	assert.True(t, isNotAttemptedErr(err), "nothing was sent, so the refusal must stay refundable (#2501)")
	assert.Empty(t, rec.snapshot(), "no prompt may reach a session whose cwd is gone")
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing)
}

// An in-place (--here) session is probed like any local worktree, but
// ArchiveSession refuses external worktrees — so its refusal must offer kill only.
func TestSendPromptWithStatus_VanishedExternalWorktreeOffersKillOnly(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "in-place")
	gone := filepath.Join(t.TempDir(), "deleted-checkout")
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, gone, "in-place", "main", "", true, false)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	require.True(t, inst.IsExternalWorktree(), "premise: the row is an in-place session")

	_, err = manager.SendPromptWithStatus(SendPromptRequest{Title: "in-place", RepoID: repoID, Prompt: "ship it"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "af sessions kill")
	assert.NotContains(t, err.Error(), "af sessions archive")
	assert.True(t, isNotAttemptedErr(err))
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

// Restore of a gone-worktree archive re-aims the record at the restore
// destination and hands the respawn a clean flag; the rebuild from the kept
// branch happens in the backend's Recover.
func TestRestoreArchived_VanishedWorktreeRepointsForRebuild(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, "vanished")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	dest, err := sessiongit.RestoreWorktreePath(repoPath, "vanished", inst.GetBranch())
	require.NoError(t, err)

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "vanished", RepoID: repoID})
	require.NoError(t, err)

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, dest, inst.GetWorktreePath(), "the record must be re-aimed at the restore destination")
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing, "restore clears the flag; the poll re-derives it if the rebuild did not land")
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

// The restore-side twin: a restore whose move landed but was never recorded.
// Git's registration follows the move, so the retry adopts the restored
// location rather than rebuilding past a live checkout of the branch.
func TestRestoreArchived_AdoptsALandedEarlierRestoreMove(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "restored", "restored")
	archivedPath := inst.GetWorktreePath()
	restoreDest, err := sessiongit.RestoreWorktreePath(repoPath, "restored", inst.GetBranch())
	require.NoError(t, err)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", archivedPath, restoreDest).CombinedOutput()
	require.NoError(t, err, string(out))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "restored", RepoID: repoID})
	require.NoError(t, err)

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, restoreDest, inst.GetWorktreePath())
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing)
	dirty, err := os.ReadFile(filepath.Join(restoreDest, "dirty.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted-restored", string(dirty))
}

// A live checkout of the branch somewhere restore would never put it is the
// user's, not af's: restore refuses rather than adopt it (kill would delete
// what it adopts) or rebuild past it.
func TestRestoreArchived_RefusesABranchCheckedOutElsewhere(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := seedArchivedSession(t, manager, repoID, repoPath, "elsewhere", "elsewhere")
	archivedPath := inst.GetWorktreePath()
	userCheckout := filepath.Join(t.TempDir(), "my-own-checkout")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", archivedPath, userCheckout).CombinedOutput()
	require.NoError(t, err, string(out))

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "elsewhere", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a location af restores to")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, archivedPath, inst.GetWorktreePath(), "nothing was adopted or repointed")
	assert.DirExists(t, userCheckout)
}
