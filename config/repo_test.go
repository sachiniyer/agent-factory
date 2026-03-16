package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveMainRepoRoot_MainRepo(t *testing.T) {
	// Create a standalone git repo so the test doesn't depend on cwd
	// (which may itself be a worktree).
	mainDir := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(mainDir, "init")
	run(mainDir, "config", "user.email", "test@test.com")
	run(mainDir, "config", "user.name", "Test")

	require.NoError(t, os.WriteFile(filepath.Join(mainDir, "file.txt"), []byte("hello"), 0644))
	run(mainDir, "add", ".")
	run(mainDir, "commit", "-m", "init")

	root, err := resolveMainRepoRoot("-C", mainDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)
}

func TestResolveMainRepoRoot_Worktree(t *testing.T) {
	// Create a temporary git repo and a linked worktree, then verify
	// that resolveMainRepoRoot from the worktree returns the main repo root.
	mainDir := t.TempDir()

	// Initialize a git repo
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
	}

	run(mainDir, "init")
	run(mainDir, "config", "user.email", "test@test.com")
	run(mainDir, "config", "user.name", "Test")

	// Create an initial commit so we can create a worktree
	dummy := filepath.Join(mainDir, "file.txt")
	require.NoError(t, os.WriteFile(dummy, []byte("hello"), 0644))
	run(mainDir, "add", ".")
	run(mainDir, "commit", "-m", "init")

	// Create a linked worktree
	wtDir := filepath.Join(t.TempDir(), "my-worktree")
	run(mainDir, "worktree", "add", wtDir, "-b", "test-branch")

	// resolveMainRepoRoot from the worktree should return mainDir
	root, err := resolveMainRepoRoot("-C", wtDir)
	require.NoError(t, err)
	assert.Equal(t, mainDir, root)

	// RepoFromPath should also resolve to the main repo
	repoFromWT, err := RepoFromPath(wtDir)
	require.NoError(t, err)
	repoFromMain, err := RepoFromPath(mainDir)
	require.NoError(t, err)
	assert.Equal(t, repoFromMain.ID, repoFromWT.ID)
	assert.Equal(t, repoFromMain.Root, repoFromWT.Root)
}

func TestRepoIDFromRoot(t *testing.T) {
	id := RepoIDFromRoot("/some/path")
	assert.Len(t, id, 12) // 6 bytes = 12 hex chars
}
