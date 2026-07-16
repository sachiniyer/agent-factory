package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runGitInPlaceTest runs a git command in dir and returns trimmed stdout,
// failing the test on error.
func runGitInPlaceTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

// TestNewGitWorktreeInPlace covers the `af sessions create --here` git layer:
// the returned worktree is attached to the repo's own working tree at its
// current branch, marked external, and neither Setup() nor Cleanup() touches
// the user's tree or branch. This reinstates the create side of the
// external-worktree capability removed in #930 PR 3, so it mirrors the
// invariants of TestCleanup_LegacyExternalWorktreeIsPreserved.
func TestNewGitWorktreeInPlace(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	runGitInPlaceTest(t, repoRoot, "checkout", "-b", "user/current-work")
	headSHA := runGitInPlaceTest(t, repoRoot, "rev-parse", "HEAD")
	branchesBefore := runGitInPlaceTest(t, repoRoot, "branch", "--list")

	gw, branch, err := NewGitWorktreeInPlace(repoRoot)
	require.NoError(t, err)

	assert.Equal(t, "user/current-work", branch, "must report the repo's current branch")
	assert.Equal(t, "user/current-work", gw.GetBranchName())
	assert.True(t, gw.IsExternalWorktree(), "an in-place worktree is user-owned and must be external")
	assert.False(t, gw.BranchCreatedByUs(), "af did not create the current branch")
	assert.Equal(t, gw.GetRepoPath(), gw.GetWorktreePath(),
		"in-place: the worktree IS the repo root")
	assert.Equal(t, normalizeWorktreePath(repoRoot), normalizeWorktreePath(gw.GetWorktreePath()))
	assert.Equal(t, headSHA, gw.GetBaseCommitSHA(), "base commit must be the current HEAD")

	// Setup must be a no-op: no new worktree registered, no branch created,
	// still on the same branch.
	require.NoError(t, gw.Setup())
	assert.Equal(t, "user/current-work",
		runGitInPlaceTest(t, repoRoot, "rev-parse", "--abbrev-ref", "HEAD"),
		"Setup must not switch branches")
	assert.Equal(t, branchesBefore, runGitInPlaceTest(t, repoRoot, "branch", "--list"),
		"Setup must not create or delete any branch")
	worktreeList := runGitInPlaceTest(t, repoRoot, "worktree", "list", "--porcelain")
	assert.Equal(t, 1, strings.Count(worktreeList, "worktree "),
		"Setup must not register a linked worktree")

	// Cleanup (the kill path) must leave the user's working tree and branch
	// intact — exactly the legacy external-worktree behavior.
	_, cleanupErrX := gw.Cleanup()
	require.NoError(t, cleanupErrX)
	_, statErr := os.Stat(repoRoot)
	require.NoError(t, statErr, "Cleanup must NOT remove the repo working tree")
	_, statErr = os.Stat(filepath.Join(repoRoot, ".git"))
	require.NoError(t, statErr, "Cleanup must NOT touch the repo's .git")
	runGitInPlaceTest(t, repoRoot, "show-ref", "--verify", "refs/heads/user/current-work")
}

// TestNewGitWorktreeInPlace_NotARepo verifies the clear-error contract:
// --here outside a git repository must fail with an actionable message.
func TestNewGitWorktreeInPlace_NotARepo(t *testing.T) {
	sandboxHome(t)

	notARepo := t.TempDir()
	_, _, err := NewGitWorktreeInPlace(notARepo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git repository",
		"error must tell the user --here needs a git repo")
}

// TestNewGitWorktreeInPlace_EmptyRepo verifies that a repo without any commit
// is rejected with the same actionable guidance the fresh-worktree path gives.
func TestNewGitWorktreeInPlace_EmptyRepo(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	_, _, err := NewGitWorktreeInPlace(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initial commit",
		"error must point at the missing initial commit")
}
