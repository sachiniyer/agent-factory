package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	log.Initialize(false)
	defer log.Close()
	os.Exit(m.Run())
}

func TestGetWorktreeDirectoryForRepo(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	worktreeDir, err := getWorktreeDirectoryForRepo(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(repoRoot), worktreeDir)
}

func TestGetWorktreeDirectoryForRepo_RequiresRepoPath(t *testing.T) {
	_, err := getWorktreeDirectoryForRepo("")
	require.Error(t, err)
}

func TestNewGitWorktree_CleanName(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)
	repoName := filepath.Base(repoRoot)

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, branchName, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	assert.Equal(t, "test/my-feature", branchName)

	expectedSuffix := repoName + "-my-feature"
	assert.True(t, strings.HasSuffix(gw.GetWorktreePath(), expectedSuffix),
		"expected worktree path to end with '%s', got: %s", expectedSuffix, gw.GetWorktreePath())

	// Should be in the parent directory of the repo
	assert.Equal(t, filepath.Dir(repoRoot), filepath.Dir(gw.GetWorktreePath()))
}

func TestNewGitWorktree_CollisionSuffix(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)
	repoName := filepath.Base(repoRoot)

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	// Create first worktree - should get clean name
	gw1, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	expectedSuffix := repoName + "-my-feature"
	assert.True(t, strings.HasSuffix(gw1.GetWorktreePath(), expectedSuffix),
		"first worktree should have clean name, got: %s", gw1.GetWorktreePath())

	// Create the directory so the next call sees a collision
	require.NoError(t, os.MkdirAll(gw1.GetWorktreePath(), 0755))

	// Create second worktree with same name - should get -2 suffix
	gw2, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gw2.GetWorktreePath(), repoName+"-my-feature-2"),
		"second worktree should have -2 suffix, got: %s", gw2.GetWorktreePath())

	// Create that directory too
	require.NoError(t, os.MkdirAll(gw2.GetWorktreePath(), 0755))

	// Create third worktree with same name - should get -3 suffix
	gw3, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gw3.GetWorktreePath(), repoName+"-my-feature-3"),
		"third worktree should have -3 suffix, got: %s", gw3.GetWorktreePath())
}

func TestSetupFromExistingBranch_SetsBaseCommitSHA(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	// Create an initial commit so HEAD is valid
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Record the HEAD commit SHA before creating the branch
	headCmd := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD")
	headOut, err := headCmd.Output()
	require.NoError(t, err)
	headSHA := strings.TrimSpace(string(headOut))

	// Create a branch manually (simulating a pre-existing branch)
	cmd = exec.Command("git", "-C", repoRoot, "branch", "test/existing-branch")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "existing-branch")
	require.NoError(t, err)

	// Setup should detect the existing branch and call setupFromExistingBranch
	err = gw.Setup()
	require.NoError(t, err)

	// The base commit SHA should be set (not empty)
	assert.NotEmpty(t, gw.GetBaseCommitSHA(), "baseCommitSHA should not be empty when reusing an existing branch")
	assert.Equal(t, headSHA, gw.GetBaseCommitSHA(), "baseCommitSHA should equal the HEAD commit")

	// Clean up
	require.NoError(t, gw.Cleanup())
}

// TestCleanup_PreservesPreExistingBranch verifies that when Setup() reuses a
// pre-existing local branch, Cleanup() does NOT delete it. Previously,
// Cleanup() always ran `git branch -D <branch>`, destroying user work on any
// branch whose name happened to match the session's derived branch name.
func TestCleanup_PreservesPreExistingBranch(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid.
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Create a pre-existing branch the user might care about.
	cmd = exec.Command("git", "-C", repoRoot, "branch", "test/preexisting")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "preexisting")
	require.NoError(t, err)

	// Setup() should detect the existing branch and reuse it.
	require.NoError(t, gw.Setup())
	assert.False(t, gw.BranchCreatedByUs(),
		"BranchCreatedByUs should be false when Setup reused an existing branch")

	// Cleanup should remove the worktree but NOT delete the branch.
	require.NoError(t, gw.Cleanup())

	// Verify the branch still exists in the repo.
	verifyCmd := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "refs/heads/test/preexisting")
	out, err = verifyCmd.CombinedOutput()
	require.NoError(t, err,
		"pre-existing branch was deleted by Cleanup; output: %s", string(out))
}

// TestCleanup_DeletesBranchWeCreated verifies that when Setup() creates a new
// branch itself, Cleanup() still deletes it (preserving existing behavior for
// branches that the session owns).
func TestCleanup_DeletesBranchWeCreated(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid.
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "brand-new")
	require.NoError(t, err)

	// No pre-existing branch — Setup() should create a fresh one.
	require.NoError(t, gw.Setup())
	assert.True(t, gw.BranchCreatedByUs(),
		"BranchCreatedByUs should be true when Setup created a new branch")

	require.NoError(t, gw.Cleanup())

	// Branch should be gone.
	verifyCmd := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "refs/heads/test/brand-new")
	err = verifyCmd.Run()
	require.Error(t, err,
		"branch created by the session should be deleted by Cleanup")
}

func TestNewGitWorktreeFromStorage_EmptyWorktreePath(t *testing.T) {
	_, err := NewGitWorktreeFromStorage("/some/repo", "", "session", "branch", "abc123", false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree path is empty")
}

func TestNewGitWorktreeFromStorage_EmptyRepoPath(t *testing.T) {
	_, err := NewGitWorktreeFromStorage("", "/some/worktree", "session", "branch", "abc123", false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo path is empty")
}

func TestNewGitWorktreeFromStorage_ValidPaths(t *testing.T) {
	gw, err := NewGitWorktreeFromStorage("/some/repo", "/some/worktree", "session", "branch", "abc123", false, true)
	require.NoError(t, err)
	assert.Equal(t, "/some/repo", gw.GetRepoPath())
	assert.Equal(t, "/some/worktree", gw.GetWorktreePath())
	assert.Equal(t, "branch", gw.GetBranchName())
	assert.Equal(t, "abc123", gw.GetBaseCommitSHA())
	assert.True(t, gw.BranchCreatedByUs())
}

func TestCleanup_EmptyRepoPath(t *testing.T) {
	gw, err := NewGitWorktreeFromStorage("/some/repo", "/some/worktree", "session", "branch", "abc123", false, true)
	require.NoError(t, err)
	// Simulate a corrupted state by zeroing out repoPath
	gw.repoPath = ""
	err = gw.Cleanup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo path is empty")
}

func TestCleanup_EmptyWorktreePath(t *testing.T) {
	gw, err := NewGitWorktreeFromStorage("/some/repo", "/some/worktree", "session", "branch", "abc123", false, true)
	require.NoError(t, err)
	// Simulate a corrupted state by zeroing out worktreePath
	gw.worktreePath = ""
	err = gw.Cleanup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree path is empty")
}

func TestFindGitRepoRoot_ResolvesLinkedWorktree(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	// Create a main repo with an initial commit (required for worktree add)
	repoRoot := createGitRepo(t)
	commitCmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "init")
	commitCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := commitCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Create a linked worktree
	linkedPath := filepath.Join(filepath.Dir(repoRoot), "linked-wt")
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", "linked-branch", linkedPath)
	out, err = addCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// findGitRepoRoot from the linked worktree should resolve to the main repo
	resolved, err := findGitRepoRoot(linkedPath)
	require.NoError(t, err)
	assert.Equal(t, repoRoot, resolved,
		"findGitRepoRoot should resolve a linked worktree back to the main repo root")
}

func TestGetWorktreeDirectoryForRepo_FromLinkedWorktree(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	// Create a main repo with an initial commit
	repoRoot := createGitRepo(t)
	commitCmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "init")
	commitCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := commitCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Create a linked worktree
	linkedPath := filepath.Join(filepath.Dir(repoRoot), "linked-wt")
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", "linked-branch", linkedPath)
	out, err = addCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// getWorktreeDirectoryForRepo from the linked worktree should return
	// the parent of the main repo, not the parent of the linked worktree.
	worktreeDir, err := getWorktreeDirectoryForRepo(linkedPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(repoRoot), worktreeDir,
		"new worktrees should be placed next to the main repo, not next to a linked worktree")
}

func createGitRepo(t *testing.T) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0755))

	cmd := exec.Command("git", "init")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return repoRoot
}
