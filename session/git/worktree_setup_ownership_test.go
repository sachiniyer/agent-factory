package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSetupRefusesWorktreeRestoredAtSelectedPath pins #4342's destructive-use
// boundary without racing. The create resolves a free path first; a differently
// titled archive then completes a real restore into that exact path before the
// create reaches Setup. The stale cleanup must refuse the changed owner rather
// than force-removing its dirty worktree.
func TestSetupRefusesWorktreeRestoredAtSelectedPath(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "tracked.txt"), []byte("base\n"), 0o644))
	runGit(t, repoRoot, "add", "tracked.txt")
	runGit(t, repoRoot, "commit", "-m", "initial")

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "af/"
	require.NoError(t, config.SaveConfig(cfg))

	const (
		archivedTitle = "feature:login"
		createTitle   = "feature-login"
	)
	archivedBranch := BranchForTitle(cfg.BranchPrefix, archivedTitle)
	createWorktree, createBranch, err := NewGitWorktree(repoRoot, createTitle, cfg.BranchPrefix)
	require.NoError(t, err)
	require.NotEqual(t, archivedBranch, createBranch)

	archivePath := filepath.Join(testguard.CanonicalTempDir(t), "archived-worktree")
	runGit(t, repoRoot, "worktree", "add", "-b", archivedBranch, archivePath)
	archivedWorktree, err := NewGitWorktreeFromStorage(
		repoRoot, archivePath, archivedTitle, archivedBranch, "", false, true,
	)
	require.NoError(t, err)

	const onlyCopy = "archive's only uncommitted copy\n"
	uncommittedPath := filepath.Join(archivePath, "only-copy.txt")
	require.NoError(t, os.WriteFile(uncommittedPath, []byte(onlyCopy), 0o644))

	restorePath, err := RestoreWorktreePath(repoRoot, archivedTitle, archivedBranch)
	require.NoError(t, err)
	require.Equal(t, createWorktree.GetWorktreePath(), restorePath,
		"the fixture must pin both operations to the same path selected while free")
	require.NoError(t, archivedWorktree.RestoreWorktreeTo(restorePath))
	uncommittedPath = filepath.Join(restorePath, "only-copy.txt")

	setupErr := createWorktree.Setup()
	assert.Error(t, setupErr, "create must refuse a worktree path whose registered branch changed")
	if setupErr != nil {
		assert.Contains(t, setupErr.Error(), restorePath)
		assert.Contains(t, setupErr.Error(), archivedBranch)
		assert.Contains(t, setupErr.Error(), createBranch)
	}
	cleanupState, cleanupErr := createWorktree.Cleanup()
	assert.Equal(t, CleanupStateUnknown, cleanupState,
		"the automatic first-create cleanup must independently re-establish branch ownership")
	assert.Error(t, cleanupErr)
	restartedCreate, err := NewGitWorktreeFromStorage(
		repoRoot, restorePath, createTitle, createBranch, "", false, true,
	)
	require.NoError(t, err)
	restartCleanupState, restartCleanupErr := restartedCreate.Cleanup()
	assert.Equal(t, CleanupStateUnknown, restartCleanupState,
		"cleanup after restart must re-establish branch ownership instead of relying on the process-local setup refusal")
	assert.Error(t, restartCleanupErr)

	got, readErr := os.ReadFile(uncommittedPath)
	require.NoError(t, readErr, "refused create must preserve the restored worktree's uncommitted file")
	assert.Equal(t, onlyCopy, string(got))

	bindings, err := WorktreeBranchBindings(repoRoot)
	require.NoError(t, err)
	var restoredStillRegistered bool
	for _, binding := range bindings {
		if normalizeWorktreePath(binding.Path) == normalizeWorktreePath(restorePath) {
			restoredStillRegistered = binding.Branch == archivedBranch && !binding.Detached
		}
	}
	assert.True(t, restoredStillRegistered,
		"restored worktree must remain registered on %s; bindings: %s", archivedBranch, formatBindings(bindings))
}

func formatBindings(bindings []WorktreeBranchBinding) string {
	var lines []string
	for _, binding := range bindings {
		lines = append(lines, binding.Path+"="+binding.Branch)
	}
	return strings.Join(lines, ", ")
}
