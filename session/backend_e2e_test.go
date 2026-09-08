package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Shared repo fixtures and a smoke check for local backend construction.

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
}

// writeLegacyRepoConfig materializes the legacy per-repo config at
// ~/.agent-factory/repos/<repoID>/config.json with the given RepoConfig.
// Production code stopped writing here after #800 pulled writes into the
// in-repo file; only test fixtures need this writer, so it lives in test scope.
func writeLegacyRepoConfig(t *testing.T, repoID string, cfg *config.RepoConfig) {
	t.Helper()
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	dir := filepath.Join(configDir, "repos", repoID)
	path := filepath.Join(dir, config.RepoConfigFileName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	data, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0644))
}

func TestE2ELocalBackendStillWorks(t *testing.T) {
	afHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	// Create a git repo with NO remote_hooks config.
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@local.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Local Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("hi"), 0644))
	runGit(t, repoDir, "add", "test.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	instance, err := NewInstance(InstanceOptions{
		Title:   "local-test",
		Path:    repoDir,
		Program: "bash",
	})
	require.NoError(t, err)
	assert.False(t, instance.Capabilities().Workspace == WorkspaceRemote, "should default to LocalBackend")
	assert.Equal(t, "local", instance.GetBackend().Type())
}
