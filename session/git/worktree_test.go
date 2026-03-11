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

func TestGetWorktreeDirectoryForRepo_Subdirectory(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	require.NoError(t, config.SaveConfig(cfg))

	worktreeDir, err := getWorktreeDirectoryForRepo(t.TempDir())
	require.NoError(t, err)

	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(configDir, "worktrees"), worktreeDir)
}

func TestGetWorktreeDirectoryForRepo_Sibling(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSibling
	require.NoError(t, config.SaveConfig(cfg))

	worktreeDir, err := getWorktreeDirectoryForRepo(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(repoRoot), worktreeDir)
}

func TestGetWorktreeDirectoryForRepo_SiblingRequiresRepoPath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSibling
	require.NoError(t, config.SaveConfig(cfg))

	_, err := getWorktreeDirectoryForRepo("")
	require.Error(t, err)
}

func TestNewGitWorktree_CleanName(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, branchName, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	assert.Equal(t, "test/my-feature", branchName)

	// Worktree path should end with the branch name, no hex suffix
	assert.True(t, strings.HasSuffix(gw.GetWorktreePath(), "test/my-feature"),
		"expected worktree path to end with 'test/my-feature', got: %s", gw.GetWorktreePath())
	// Should NOT contain an underscore followed by hex (old format)
	base := filepath.Base(gw.GetWorktreePath())
	assert.False(t, strings.Contains(base, "_"),
		"worktree path should not contain underscore hex suffix, got: %s", base)
}

func TestNewGitWorktree_CollisionSuffix(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	// Create first worktree - should get clean name
	gw1, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gw1.GetWorktreePath(), "test/my-feature"),
		"first worktree should have clean name, got: %s", gw1.GetWorktreePath())

	// Create the directory so the next call sees a collision
	require.NoError(t, os.MkdirAll(gw1.GetWorktreePath(), 0755))

	// Create second worktree with same name - should get -2 suffix
	gw2, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gw2.GetWorktreePath(), "test/my-feature-2"),
		"second worktree should have -2 suffix, got: %s", gw2.GetWorktreePath())

	// Create that directory too
	require.NoError(t, os.MkdirAll(gw2.GetWorktreePath(), 0755))

	// Create third worktree with same name - should get -3 suffix
	gw3, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gw3.GetWorktreePath(), "test/my-feature-3"),
		"third worktree should have -3 suffix, got: %s", gw3.GetWorktreePath())
}

func TestNewGitWorktree_SiblingCleanName(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)
	repoName := filepath.Base(repoRoot)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSibling
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	expectedSuffix := repoName + "-my-feature"
	assert.True(t, strings.HasSuffix(gw.GetWorktreePath(), expectedSuffix),
		"sibling worktree should end with '%s', got: %s", expectedSuffix, gw.GetWorktreePath())

	// Should be in the parent directory of the repo
	assert.Equal(t, filepath.Dir(repoRoot), filepath.Dir(gw.GetWorktreePath()))
}

func TestNewGitWorktree_SiblingCollision(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	repoRoot := createGitRepo(t)
	repoName := filepath.Base(repoRoot)

	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSibling
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw1, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(gw1.GetWorktreePath(), 0755))

	gw2, _, err := NewGitWorktree(repoRoot, "my-feature")
	require.NoError(t, err)

	expectedSuffix := repoName + "-my-feature-2"
	assert.True(t, strings.HasSuffix(gw2.GetWorktreePath(), expectedSuffix),
		"sibling collision worktree should end with '%s', got: %s", expectedSuffix, gw2.GetWorktreePath())
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
