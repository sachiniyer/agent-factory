package app

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #6233: the delete dialog must not promise loss for a branch that survives. When
// the worktree's HEAD is on a branch af does not own, the dialog names it and
// says it and its commits are kept — read from the worktree, not the recorded name.
func TestKillConfirmNamesAUserBranchThatSurvives(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "dev/beta"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"checkout", "-q", "-b", "feature/done-flag"},
	} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	impact := session.WorktreeCleanupImpact{Path: dir, Branch: "dev/beta", RemoveWorktree: true, DeleteBranch: true}
	loss := assessKillLoss(impact)
	msg := killConfirmMessage("beta", joinWarnings(loss.warnings), false, &impact)
	assert.Contains(t, msg, "'feature/done-flag'")
	assert.Contains(t, msg, "kept in the repo")

	out, err := exec.Command("git", "-C", dir, "checkout", "-q", "dev/beta").CombinedOutput()
	require.NoError(t, err, string(out))
	msg = killConfirmMessage("beta", joinWarnings(assessKillLoss(impact).warnings), false, &impact)
	assert.NotContains(t, msg, "kept in the repo", "HEAD on the owned branch: nothing extra survives")
}

// An unreadable HEAD is said, never guessed.
func TestKillConfirmSaysWhenTheWorktreeBranchCannotBeRead(t *testing.T) {
	impact := session.WorktreeCleanupImpact{Path: t.TempDir(), Branch: "dev/beta", RemoveWorktree: true, DeleteBranch: true}
	msg := killConfirmMessage("beta", joinWarnings(assessKillLoss(impact).warnings), false, &impact)
	assert.Contains(t, msg, "Could not read the worktree's current branch")
}
