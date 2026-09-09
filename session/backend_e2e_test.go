package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
