package daemon

import (
	"os"
	"os/exec"
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
