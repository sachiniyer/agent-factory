package git

import (
	"bytes"
	"fmt"
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME so
	// stray config/state/log writes land in a temp dir instead of the
	// developer's real one. Sandbox AFTER the tripwire snapshots the real
	// environment, BEFORE logging resolves its file path.
	restoreHome := testguard.SandboxHome()
	log.Initialize(false)
	code := m.Run()
	log.Close()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// sandboxHome points HOME and AGENT_FACTORY_HOME at a fresh temp dir.
// Overriding HOME alone is not enough: config.GetConfigDir prefers
// AGENT_FACTORY_HOME, so in any environment that exports it, the
// config.SaveConfig calls below would write into the user's real config
// dir (#837).
func sandboxHome(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(tempHome, ".agent-factory"))
}

func TestGetWorktreeDirectoryForRepo(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	worktreeDir, err := getWorktreeDirectoryForRepo(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(repoRoot), worktreeDir)
}

func TestGetWorktreeDirectoryForRepoSubdirectory(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	require.NoError(t, config.SaveConfig(cfg))

	worktreeDir, err := getWorktreeDirectoryForRepo(repoRoot)
	require.NoError(t, err)
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "worktrees"), worktreeDir)
}

func TestGetWorktreeDirectoryForRepo_RequiresRepoPath(t *testing.T) {
	_, err := getWorktreeDirectoryForRepo("")
	require.Error(t, err)
}

func TestNewGitWorktree_CleanName(t *testing.T) {
	sandboxHome(t)

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

func TestNewGitWorktree_SubdirectoryCleanName(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	require.NoError(t, config.SaveConfig(cfg))

	gw, branchName, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	assert.Equal(t, "test/my-feature", branchName)
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "worktrees", "test", "my-feature"), gw.GetWorktreePath())
}

func TestNewGitWorktree_CollisionSuffix(t *testing.T) {
	sandboxHome(t)

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

func TestNewGitWorktree_StatErrorReturns(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	errCh := make(chan error, 1)
	go func() {
		_, _, err := NewGitWorktree(repoRoot, strings.Repeat("a", 300))
		errCh <- err
	}()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot check worktree path")
	case <-time.After(2 * time.Second):
		t.Fatal("NewGitWorktree hung on a non-ENOENT stat error")
	}
}

func TestSetupFromExistingBranch_SetsBaseCommitSHA(t *testing.T) {
	sandboxHome(t)

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

// TestSetupFromExistingBranch_RecreatesAfterExternalDeletion is a regression
// test for issue #695. When a worktree directory is deleted outside the tool
// (rm -rf, disk cleanup, etc.) git keeps tracking the worktree internally, and
// a subsequent `git worktree add <same-path>` can fail with "missing but
// already registered worktree". This guards the user-facing contract: reusing
// a session name whose worktree directory was deleted externally must recreate
// the worktree successfully.
//
// setupFromExistingBranch recovers via `worktree remove -f` followed by
// `worktree prune` (added for this fix) before re-adding. Recent git clears a
// missing-but-registered worktree on `remove -f` alone, so this test passes
// even without the prune on such versions; the prune mirrors setupNewWorktree
// and covers older git where `remove` errors on a missing worktree and leaves
// the stale registration that blocks `worktree add`.
func TestSetupFromExistingBranch_RecreatesAfterExternalDeletion(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid.
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Pre-existing branch so Setup() takes the setupFromExistingBranch path.
	cmd = exec.Command("git", "-C", repoRoot, "branch", "test/existing-branch")
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "existing-branch")
	require.NoError(t, err)

	// First setup succeeds and registers the worktree.
	require.NoError(t, gw.Setup())
	worktreePath := gw.GetWorktreePath()

	// Simulate the user deleting the worktree directory out from under git
	// (rm -rf). The registration in .git/worktrees/<name> survives.
	require.NoError(t, os.RemoveAll(worktreePath))

	// Recreating the session reuses the same path. Without the prune this
	// fails with "missing but already registered worktree".
	require.NoError(t, gw.Setup(),
		"recreating a worktree after its directory was deleted externally should succeed")

	// The worktree directory should exist again.
	_, err = os.Stat(worktreePath)
	require.NoError(t, err, "worktree directory should be recreated")

	require.NoError(t, gw.Cleanup())
}

// TestCleanup_PreservesPreExistingBranch verifies that when Setup() reuses a
// pre-existing local branch, Cleanup() does NOT delete it. Previously,
// Cleanup() always ran `git branch -D <branch>`, destroying user work on any
// branch whose name happened to match the session's derived branch name.
func TestCleanup_PreservesPreExistingBranch(t *testing.T) {
	sandboxHome(t)

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
	sandboxHome(t)

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

// TestCleanup_LegacyExternalWorktreeIsPreserved is the #930 PR 3 back-compat
// safety. PR 3 removed the create-on-existing-worktree feature, but instances
// persisted by the OLD feature carry externalWorktree=true /
// branchCreatedByUs=false on disk. When such a record is restored via
// NewGitWorktreeFromStorage and later killed, Cleanup() must remain a no-op:
// it must NOT remove the user's worktree directory or delete their branch.
// Removing the legacy field handling now would destroy user data on kill — so
// it stays until a future PR confirms no persisted instance carries it.
func TestCleanup_LegacyExternalWorktreeIsPreserved(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)

	// Initial commit so HEAD is valid.
	commitCmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	commitCmd.Env = env
	out, err := commitCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// A user-owned branch + worktree, exactly what the removed feature would
	// have attached an instance to.
	branchCmd := exec.Command("git", "-C", repoRoot, "branch", "user/keep-me")
	out, err = branchCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	externalWtPath := filepath.Join(t.TempDir(), "external-wt")
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", externalWtPath, "user/keep-me")
	out, err = addCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Restore the instance from disk the way FromInstanceData does for a record
	// written by the old feature: externalWorktree=true, branchCreatedByUs=false.
	gw, err := NewGitWorktreeFromStorage(
		repoRoot, externalWtPath, "legacy", "user/keep-me", "",
		true,  // externalWorktree (legacy)
		false, // branchCreatedByUs
	)
	require.NoError(t, err)
	require.True(t, gw.IsExternalWorktree(), "restored legacy worktree must report external")

	// Cleanup must be a no-op for an external worktree.
	require.NoError(t, gw.Cleanup())

	// The worktree directory must still exist.
	_, statErr := os.Stat(externalWtPath)
	require.NoError(t, statErr,
		"external worktree directory must NOT be removed by Cleanup (#930 PR 3 back-compat)")

	// The branch must still exist.
	verifyCmd := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "refs/heads/user/keep-me")
	out, err = verifyCmd.CombinedOutput()
	require.NoError(t, err,
		"user-owned branch must NOT be deleted by Cleanup; output: %s", string(out))
}

// TestCleanup_PrunesBeforeBranchDelete is a regression test for #611. When
// `git worktree remove -f` fails (e.g. the worktree's `.git` pointer file
// has been removed externally), git retains internal worktree metadata.
// Without an intervening `git worktree prune`, `git branch -D` reports the
// branch is "in use" and the orphaned branch is left behind.
// CleanupWorktreesForRepo already prunes before branch deletion (#330);
// this verifies GitWorktree.Cleanup() follows the same order.
func TestCleanup_PrunesBeforeBranchDelete(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid (required for `worktree add`).
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

	gw, branchName, err := NewGitWorktree(repoRoot, "corrupted")
	require.NoError(t, err)

	// Setup() creates a fresh branch and worktree owned by this session.
	require.NoError(t, gw.Setup())
	require.True(t, gw.BranchCreatedByUs(),
		"BranchCreatedByUs should be true when Setup created a new branch")

	// Corrupt the worktree by removing its `.git` pointer file. This makes
	// `git worktree remove -f` fail validation; without the prune-before-delete
	// fix, git still tracks the worktree internally and blocks `branch -D`.
	require.NoError(t, os.Remove(filepath.Join(gw.GetWorktreePath(), ".git")))

	// Cleanup may return errors from the failed `worktree remove`, but it
	// must still delete the branch — that's the user-visible regression.
	_ = gw.Cleanup()

	// The branch should be gone — this is what regresses without the prune
	// before `git branch -D`.
	branchCmd := exec.Command("git", "-C", repoRoot, "branch", "--list", branchName)
	out, err = branchCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)),
		"session-owned branch should be deleted even when `git worktree remove` fails on a corrupted worktree")

	// Only the main worktree should remain — no stale linked-worktree metadata.
	listCmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err = listCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, 1, strings.Count(string(out), "worktree "),
		"only the main worktree should remain, got:\n%s", string(out))
}

// TestCleanup_RemovesOrphanedDirectory is a regression test for #719. When the
// worktree's `.git` pointer is corrupted, `git worktree remove -f` fails with
// "validation failed" and leaves the directory on disk. Cleanup() must fall
// back to os.RemoveAll so the orphaned directory does not leak disk space and
// force the user into `af reset`. CleanupWorktreesForRepo already does this;
// this verifies GitWorktree.Cleanup() follows the same fallback.
func TestCleanup_RemovesOrphanedDirectory(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid (required for `worktree add`).
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

	gw, _, err := NewGitWorktree(repoRoot, "orphan")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	worktreePath := gw.GetWorktreePath()

	// Corrupt the worktree by removing its `.git` pointer file so that
	// `git worktree remove -f` fails validation and leaves the directory.
	require.NoError(t, os.Remove(filepath.Join(worktreePath, ".git")))

	// Cleanup may report errors, but it must remove the on-disk directory.
	_ = gw.Cleanup()

	_, err = os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(err),
		"Cleanup() must remove the orphaned worktree directory when `git worktree remove` fails validation")
}

// TestCleanup_RemovesDirWhenGitDeregistered is the regression test for #802.
// When `git worktree remove -f` fails AFTER git has already released the
// registration — observed in real usage when the dying agent process wrote
// into the tree mid-removal, so git deregistered the worktree and then its
// recursive delete aborted with "Directory not empty" — the directory must
// not leak. Cleanup() decides by ownership, not error strings: the path is
// absent from `git worktree list`, so it falls back to os.RemoveAll.
//
// Simulated here by deleting the worktree's admin entry
// (.git/worktrees/<name>), which produces the same post-failure state with
// real git: `worktree remove` fails ("is not a working tree") while the
// registration is already gone and the directory is fully populated on disk.
func TestCleanup_RemovesDirWhenGitDeregistered(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

	// Initial commit so HEAD is valid (required for `worktree add`).
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

	gw, branchName, err := NewGitWorktree(repoRoot, "deregistered")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	worktreePath := gw.GetWorktreePath()

	// Drop the worktree's admin entry so git no longer registers the path
	// while the directory itself survives on disk untouched.
	adminDir := filepath.Join(repoRoot, ".git", "worktrees", filepath.Base(worktreePath))
	require.NoError(t, os.RemoveAll(adminDir))

	// Sanity: git has let go, the directory is still there.
	registered, err := gw.isWorktreeRegistered()
	require.NoError(t, err)
	require.False(t, registered, "worktree should be deregistered after its admin entry is removed")
	_, err = os.Stat(worktreePath)
	require.NoError(t, err)

	// Full recovery is expected: no error, directory gone, branch gone.
	require.NoError(t, gw.Cleanup())

	_, err = os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(err),
		"Cleanup() must remove the worktree directory once git has deregistered it (#802)")

	branchCmd := exec.Command("git", "-C", repoRoot, "branch", "--list", branchName)
	out, err = branchCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)),
		"session-owned branch should be deleted after the deregistered worktree is cleaned up")
}

// TestCleanup_SurfacesErrorWhenGitStillOwnsWorktree is the safety counterpart
// to the #802 fallback: when git still registers the worktree and the removal
// failure is not the known #726 "validation failed" class, Cleanup() must
// surface the error and leave the directory alone instead of deleting data it
// cannot account for. A locked worktree is the simplest deterministic member
// of that class: a single `-f` refuses to remove it and the registration
// stays put.
func TestCleanup_SurfacesErrorWhenGitStillOwnsWorktree(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)

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

	gw, _, err := NewGitWorktree(repoRoot, "stillowned")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	worktreePath := gw.GetWorktreePath()

	lockCmd := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err = lockCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	err = gw.Cleanup()
	require.Error(t, err,
		"Cleanup() must surface the failure when git still owns the worktree and the cause is unknown")
	assert.Contains(t, err.Error(), "locked")

	// The directory must survive — it is still a registered git worktree.
	_, statErr := os.Stat(worktreePath)
	assert.NoError(t, statErr,
		"Cleanup() must NOT delete a directory git still registers as a worktree")

	registered, regErr := gw.isWorktreeRegistered()
	require.NoError(t, regErr)
	assert.True(t, registered)
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
	sandboxHome(t)

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
	sandboxHome(t)

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

// TestIsPathStrictlyInside covers the containment check used to validate that
// a derived worktree path lives under the configured worktree directory. The
// `/`-root cases exercise the fix for #461.
func TestIsPathStrictlyInside(t *testing.T) {
	cases := []struct {
		name    string
		absBase string
		absDir  string
		want    bool
	}{
		{"nested under home", "/home/user/repo-session", "/home/user", true},
		{"nested under root", "/repo-session", "/", true},
		{"sibling directory", "/home/user2/foo", "/home/user", false},
		{"equal to dir", "/home/user", "/home/user", false},
		{"equal to root", "/", "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPathStrictlyInside(tc.absBase, tc.absDir)
			assert.Equal(t, tc.want, got,
				"isPathStrictlyInside(%q, %q)", tc.absBase, tc.absDir)
		})
	}
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

// TestCleanupWorktreesForRepo_PrunesBeforeBranchDelete is a regression test
// for issue #330. When `git worktree remove -f` fails (e.g. the worktree's
// `.git` pointer file has been removed externally) and CleanupWorktreesForRepo
// falls back to os.RemoveAll, git still tracks the worktree in its metadata.
// Without an intervening `git worktree prune`, `git branch -D` reports the
// branch is "in use" and the orphaned branch is left behind.
func TestCleanupWorktreesForRepo_PrunesBeforeBranchDelete(t *testing.T) {
	sandboxHome(t)

	repoRoot := createGitRepo(t)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	commitCmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "init")
	commitCmd.Env = env
	out, err := commitCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	linkedPath := filepath.Join(filepath.Dir(repoRoot), "linked-wt")
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", "linked-branch", linkedPath)
	addCmd.Env = env
	out, err = addCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Corrupt the worktree by removing its `.git` pointer file. This makes
	// `git worktree remove -f` fail validation, forcing the os.RemoveAll
	// fallback path. Git's internal metadata still references the worktree
	// until `git worktree prune` runs.
	require.NoError(t, os.Remove(filepath.Join(linkedPath, ".git")))

	// Cleanup should still complete successfully and delete the branch.
	require.NoError(t, CleanupWorktreesForRepo(repoRoot))

	// The branch should be gone — this is what regresses without the prune
	// before `git branch -D`.
	branchCmd := exec.Command("git", "-C", repoRoot, "branch", "--list", "linked-branch")
	out, err = branchCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)),
		"linked branch should be deleted even when `git worktree remove` falls back to os.RemoveAll")

	// Only the main worktree should remain.
	listCmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err = listCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, 1, strings.Count(string(out), "worktree "),
		"only the main worktree should remain, got:\n%s", string(out))
}

// TestCleanupWorktreesForRepo_RejectsEmpty verifies the exported helper does
// not silently operate on the cwd when given an empty repo root.
func TestCleanupWorktreesForRepo_RejectsEmpty(t *testing.T) {
	err := CleanupWorktreesForRepo("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo root is empty")
}

// TestCleanupWorktreesForRepo_CleansGivenRepo verifies that
// CleanupWorktreesForRepo targets the repo it is given — regardless of the
// process's current working directory. This is the core of the #265 fix:
// `af reset` must be able to clean worktrees in repos OTHER than the cwd.
func TestCleanupWorktreesForRepo_CleansGivenRepo(t *testing.T) {
	sandboxHome(t)

	// Build a repo, make an initial commit, and add a linked worktree.
	repoRoot := createGitRepo(t)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	commitCmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "init")
	commitCmd.Env = env
	out, err := commitCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	linkedPath := filepath.Join(filepath.Dir(repoRoot), "linked-wt")
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", "linked-branch", linkedPath)
	addCmd.Env = env
	out, err = addCmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// Sanity: the linked worktree is on disk and known to git.
	_, err = os.Stat(linkedPath)
	require.NoError(t, err)

	// Run cleanup targeting this repo EXPLICITLY. We do not chdir, so if
	// the helper were derived from cwd (the old behavior) it would not
	// touch this repo at all.
	require.NoError(t, CleanupWorktreesForRepo(repoRoot))

	// The linked worktree directory should have been removed.
	if _, statErr := os.Stat(linkedPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected linked worktree to be removed; got stat err: %v", statErr)
	}

	// The linked branch should have been deleted.
	branchCmd := exec.Command("git", "-C", repoRoot, "branch", "--list", "linked-branch")
	out, err = branchCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)), "linked branch should be deleted")

	// Only the main worktree should remain.
	listCmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	out, err = listCmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, 1, strings.Count(string(out), "worktree "),
		"only the main worktree should remain, got:\n%s", string(out))
}

// TestCleanupWorktreesForRepo_SkipsMissingPath verifies that when a stored
// repo root no longer exists on disk (e.g. because the user moved or deleted
// the repo), CleanupWorktreesForRepo logs a warning and returns nil instead
// of aborting `af reset`. See issue #341.
func TestCleanupWorktreesForRepo_SkipsMissingPath(t *testing.T) {
	// Redirect WarningLog to a buffer so we can assert on the message.
	var buf bytes.Buffer
	origWarning := log.WarningLog
	log.WarningLog = stdlog.New(&buf, "WARNING: ", 0)
	t.Cleanup(func() { log.WarningLog = origWarning })

	missing := filepath.Join(t.TempDir(), "definitely-does-not-exist")
	// Sanity: the path really is absent.
	_, statErr := os.Stat(missing)
	require.True(t, os.IsNotExist(statErr), "test setup: path should not exist")

	err := CleanupWorktreesForRepo(missing)
	require.NoError(t, err,
		"CleanupWorktreesForRepo should return nil when the repo path is missing")

	assert.Contains(t, buf.String(), "skipping cleanup for deleted repo",
		"expected warning log about skipped cleanup, got: %q", buf.String())
}

// TestCleanupWorktreesForRepo_SkipsNonGitPath verifies that when a stored repo
// path exists but is no longer a git repo (e.g. `.git` has been removed),
// CleanupWorktreesForRepo logs a warning and returns nil instead of aborting
// `af reset`. See issue #370.
func TestCleanupWorktreesForRepo_SkipsNonGitPath(t *testing.T) {
	// Redirect WarningLog to a buffer so we can assert on the message.
	var buf bytes.Buffer
	origWarning := log.WarningLog
	log.WarningLog = stdlog.New(&buf, "WARNING: ", 0)
	t.Cleanup(func() { log.WarningLog = origWarning })

	// Create a directory that exists but is not a git repo.
	nonGit := t.TempDir()
	// Sanity: the directory exists but has no .git entry.
	_, statErr := os.Stat(nonGit)
	require.NoError(t, statErr, "test setup: path should exist")
	_, gitStatErr := os.Stat(filepath.Join(nonGit, ".git"))
	require.True(t, os.IsNotExist(gitStatErr), "test setup: .git should not exist")

	err := CleanupWorktreesForRepo(nonGit)
	require.NoError(t, err,
		"CleanupWorktreesForRepo should return nil when the path is not a git repo")

	assert.Contains(t, buf.String(), "skipping cleanup for non-git path",
		"expected warning log about non-git path, got: %q", buf.String())
}
