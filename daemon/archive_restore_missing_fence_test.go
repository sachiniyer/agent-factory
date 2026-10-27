package daemon

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// archiveGoneRowForRestore archives a session whose worktree was deleted
// outside af and returns the instance and the directory a restore of it places
// — rebuilt from the branch, or, when ownRebuilt, an earlier restore's rebuild
// that this one adopts.
func archiveGoneRowForRestore(t *testing.T, manager *Manager, repoID, repoPath, title string, ownRebuilt bool) (*session.Instance, string) {
	t.Helper()
	inst, _ := registerGoneWorktree(t, manager, repoID, repoPath, title)
	branch := "af/" + sanitizeArchiveTitle(title)
	inst.SetBranchForTest(branch)
	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.True(t, archived.Worktree.Missing, "premise: the archive recorded the deletion")
	dest, err := sessiongit.RestoreWorktreeBasePath(repoPath, title, branch)
	require.NoError(t, err)
	if ownRebuilt {
		gitForTest(t, repoPath, "worktree", "prune")
		gitForTest(t, repoPath, "worktree", "add", dest, branch)
	}
	return inst, dest
}

// A placed restore directory's identity is pinned only in memory, and the
// restore's first commit writes the placed path to disk BEFORE the
// respawn-boundary reconfirmation. That write must therefore not be the one
// that drops the worktree-missing flag: a daemon that exits after it would
// restart on a row claiming an unverified directory is the session's worktree,
// and a later restore would move or start the agent in whatever a same-UID
// process swapped in there. Every write after the first commit fails here,
// which leaves disk holding exactly what a crash at that point leaves (#5102).
func TestRestoreArchived_CrashAfterTheFirstCommitKeepsTheRowFlagged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ownRebuilt bool
	}{
		{name: "fresh rebuild", ownRebuilt: false},
		{name: "adopted earlier rebuild", ownRebuilt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			title := "commit-crash"
			if tc.ownRebuilt {
				title = "commit-crash-adopt"
			}
			_, dest := archiveGoneRowForRestore(t, manager, repoID, repoPath, title, tc.ownRebuilt)

			// Locked because this restore respawns the agent, and anything that
			// persists the row afterwards does so off the test goroutine.
			var mu sync.Mutex
			var firstCommit *session.InstanceData
			prev := testHookPersistInstanceData
			t.Cleanup(func() { testHookPersistInstanceData = prev })
			testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
				if data.Title != title {
					return nil
				}
				mu.Lock()
				defer mu.Unlock()
				if firstCommit != nil {
					return errors.New("the daemon exited after the first restore commit")
				}
				if data.Worktree.WorktreePath == dest {
					captured := data
					firstCommit = &captured
				}
				return nil
			}

			// The outcome is whatever the cut-off writes make it; only disk matters.
			_, _, _ = manager.RestoreArchived(RestoreArchivedRequest{Title: title, RepoID: repoID})

			mu.Lock()
			got := firstCommit
			mu.Unlock()
			require.NotNil(t, got, "premise: the restore reached its first commit")
			assert.True(t, got.Worktree.Missing, "the first commit must keep the fence it was written under")
			rec := recordFor(t, repoID, title)
			require.NotNil(t, rec)
			assert.Equal(t, session.LiveArchived, rec.Liveness)
			assert.Equal(t, dest, rec.Worktree.WorktreePath, "premise: the placed path reached disk")
			assert.True(t, rec.Worktree.Missing,
				"a restart must find the placed directory fenced, not recorded as the verified worktree")
			assert.Equal(t, session.WorktreeMissingPlacingReason(dest), rec.Worktree.MissingReason)
		})
	}
}

// A swap that only the respawn-boundary reconfirmation catches is refused with
// the flag still on disk. The directory is swapped as the first commit writes,
// so the reconfirmation before the commit passes and the one before the respawn
// is the first to see it; the record it persists is fenced by the git layer
// AND still flagged, since nothing verified the stranger (#5102).
func TestRestoreArchived_PlacedWorktreeSwappedBeforeTheRespawnStaysFlagged(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	title := "respawn-swap"
	inst, dest := archiveGoneRowForRestore(t, manager, repoID, repoPath, title, false)

	var marker string
	var swapErr error
	swapped := false
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != title || swapped || data.Worktree.WorktreePath != dest {
			return nil
		}
		swapped = true
		marker, swapErr = swapDirectory(dest)
		return nil
	}

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: title, RepoID: repoID})
	require.NoError(t, swapErr)
	require.NotEmpty(t, marker, "premise: the first commit ran and the swap with it")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer the directory it verified")
	assert.Contains(t, err.Error(), "before starting the agent in it", "premise: the respawn boundary is what caught it")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
	assert.FileExists(t, marker, "the stranger's files are untouched")
	missing, _ := inst.WorktreeMissing()
	assert.True(t, missing)
	rec := recordFor(t, repoID, title)
	require.NotNil(t, rec)
	assert.Equal(t, session.LiveArchived, rec.Liveness)
	assert.True(t, rec.Worktree.Missing, "the flag clears only once the respawn boundary verified the directory")
	require.NotNil(t, rec.Worktree.RelocationRecovery, "the fence reached disk")
	assert.NotZero(t, rec.Worktree.RelocationRecovery.Inode)
}
