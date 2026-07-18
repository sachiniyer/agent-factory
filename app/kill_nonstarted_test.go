package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// #2029: the committed-unmerged-work warning (#2022) was gated behind
// GetGitWorktree(), which errors when started=false. So a session that has a
// worktree but was never started — e.g. one whose restore failed — got the bare,
// safe-looking "[!] Kill session 'x'?" prompt with no data-loss warning, even
// though killing still force-deletes its branch and orphans the same committed
// work. The dirty-worktree check (via GetWorktreePath) was NOT so gated, so the two
// checks covered different session states. The fix runs both under the ungated
// GetWorktreePath / GetBaseCommitSHA accessors.

// nonStartedWorktreeInstance builds an instance with a real git worktree that is
// NOT started (the restore-failed shape #2029 is about): GetGitWorktree errors for
// it, but GetWorktreePath / GetBaseCommitSHA still resolve. Status is set to a
// non-creating, non-tearing-down value so handleKill opens the confirmation.
func nonStartedWorktreeInstance(t *testing.T, title, repoDir, worktreePath, branch, baseSHA string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoDir, Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Ready) // clears OpCreating; started stays false
	gw, err := git.NewGitWorktreeFromStorage(repoDir, worktreePath, title, branch, baseSHA, false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// TestHandleKill_NonStarted_UnmergedCommit_StillWarns is the #2029 regression: a
// non-started session whose branch carries an unmerged, unpushed commit must still
// get the loud data-loss warning and the distinct confirm key — the same
// protection a started session gets (#2022), because the kill destroys the same
// work.
func TestHandleKill_NonStarted_UnmergedCommit_StillWarns(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/nonstarted")
	commitInWorktree(t, wt)
	require.Empty(t, killGit(t, wt, "status", "--porcelain"), "worktree must be clean (only committed work at risk)")

	inst := nonStartedWorktreeInstance(t, "nonstarted", repoDir, wt, "dev/nonstarted", baseSHA)

	// Preconditions that make this the #2029 case: not started, and the OLD gate
	// (GetGitWorktree) errors — so the pre-fix code skipped the unmerged check —
	// while the ungated accessors the fix uses still resolve.
	require.False(t, inst.Started(), "the session must be non-started")
	_, gwErr := inst.GetGitWorktree()
	require.Error(t, gwErr, "GetGitWorktree is started-gated; the old code skipped the unmerged check for this session")
	require.NotEmpty(t, inst.GetWorktreePath(), "the ungated worktree-path accessor must still resolve")
	require.Equal(t, baseSHA, inst.GetBaseCommitSHA(), "the ungated base-SHA accessor must still resolve")

	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "1 commit",
		"the unmerged-work warning must compute for a session that has a worktree even if never started (#2029)")
	assert.Contains(t, rendered, "cannot be undone")
	assert.NotContains(t, rendered, "uncommitted changes", "the loss is committed work, not a dirty worktree")
	require.Equal(t, unmergedKillConfirmKey, hm.confirmationOverlay.ConfirmKey,
		"a non-started session's unrecoverable committed work must still escalate the confirm key, not the ordinary 'y'")
}

// TestHandleKill_NonStarted_CleanLevelBranch_KeepsBareConfirmation guards the
// other direction: a non-started session with nothing to lose (branch level with
// base) must still kill behind the bare confirmation and ordinary 'y' — the
// ungating must not manufacture a false warning.
func TestHandleKill_NonStarted_CleanLevelBranch_KeepsBareConfirmation(t *testing.T) {
	repoDir, baseSHA := initBaseRepo(t)
	wt := addWorktree(t, repoDir, baseSHA, "dev/nonstarted-empty") // no commit beyond base

	inst := nonStartedWorktreeInstance(t, "nonstarted-empty", repoDir, wt, "dev/nonstarted-empty", baseSHA)
	require.False(t, inst.Started())

	_, hm := armKill(t, inst)

	rendered := flatten(hm.confirmationOverlay.Render())
	assert.Contains(t, rendered, "Kill session 'nonstarted-empty'?")
	assert.NotContains(t, rendered, "commit", "a level branch must not warn about commits")
	assert.Equal(t, "y", hm.confirmationOverlay.ConfirmKey,
		"a non-started session with nothing to lose must keep the ordinary 'y' confirm")
}
