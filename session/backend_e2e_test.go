package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Backend-resolution E2E for the remote-hook backend (#1592 Phase 4 PR7). The
// full remote LIFECYCLE (provision → drive over wss → teardown) is exercised by
// integration/remote_roundtrip_test.go against a REAL af agent-server behind a
// mock launch_cmd; here we cover the resolution-layer contract that does not
// need a live sandbox: config validity + repo-relative path rewriting (#834).

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
}

func writeE2EScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0755))
	return path
}

func TestE2ELocalBackendStillWorks(t *testing.T) {
	afHome := t.TempDir()
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

// TestE2EBackendResolutionRejectsEmptyHookCommands is the resolution-layer
// regression test for #738, updated for the provision-and-expose contract
// (#1592 Phase 4 PR7): a remote_hooks config missing launch_cmd must fail fast
// at backend resolution with an actionable error naming the field, rather than
// constructing a backend that later dies with exec's cryptic "exec: no command".
func TestE2EBackendResolutionRejectsEmptyHookCommands(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@empty.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Empty Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "f.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	cfg := &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: "",
			DeleteCmd: "/bin/echo",
		},
	}
	require.NoError(t, config.SaveRepoConfig(repo.ID, cfg))

	// loadRemoteHooksForPath must reject an empty launch_cmd.
	_, err = loadRemoteHooksForPath(repoDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch_cmd")
}

// setupE2ERelativeHooksRepo creates a real git repo whose in-repo config
// declares remote_hooks as repo-relative paths (#834), with the launch/delete
// hook scripts checked into the repo under .agent-factory/hooks/. Returns the
// repo dir.
func setupE2ERelativeHooksRepo(t *testing.T) string {
	t.Helper()

	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@rel.com")
	runGit(t, repoDir, "config", "--local", "user.name", "Rel Test")

	hooksDir := filepath.Join(repoDir, config.InRepoConfigDirName, "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0755))

	writeE2EScript(t, hooksDir, "launch.sh", `echo '{"url":"wss://127.0.0.1:9","token":"t","tls_fingerprint":"fp"}'`)
	writeE2EScript(t, hooksDir, "delete.sh", `echo '{"deleted": true}'`)

	// The whole point: the config carries repo-relative commands.
	require.NoError(t, os.WriteFile(config.InRepoConfigPath(repoDir), []byte(`{
  "remote_hooks": {
    "launch_cmd": "./.agent-factory/hooks/launch.sh",
    "delete_cmd": "./.agent-factory/hooks/delete.sh"
  }
}`), 0644))

	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-m", "init with relative remote_hooks")

	return repoDir
}

// TestE2ERemoteHooksRelativePaths is the acceptance test for #834: an in-repo
// remote_hooks config written with repo-relative paths must resolve to absolute
// paths under the repo root before any exec, even though the process cwd is not
// the repo root (the daemon's cwd is unrelated to the repo).
func TestE2ERemoteHooksRelativePaths(t *testing.T) {
	repoDir := setupE2ERelativeHooksRepo(t)

	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, repo.Root, cwd, "test must run with cwd outside the repo for the relative paths to be meaningful")

	// Backend resolution rewrites the commands to absolute paths under the repo
	// root before they reach any exec site.
	hooks, err := loadRemoteHooksForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/launch.sh"), hooks.LaunchCmd)
	assert.Equal(t, filepath.Join(repo.Root, ".agent-factory/hooks/delete.sh"), hooks.DeleteCmd)
}

// TestE2ERemoteHooksRelativePathsLinkedWorktree pins the linked-worktree rule
// from #834: relative hook paths resolve against the main repository root — the
// root whose .agent-factory/config.json was loaded — never against the linked
// worktree's own path.
func TestE2ERemoteHooksRelativePathsLinkedWorktree(t *testing.T) {
	repoDir := setupE2ERelativeHooksRepo(t)

	worktreeDir := filepath.Join(t.TempDir(), "linked-wt")
	runGit(t, repoDir, "worktree", "add", worktreeDir, "-b", "wt-branch")

	repo, err := config.RepoFromPath(worktreeDir)
	require.NoError(t, err)
	mainRepo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	require.Equal(t, mainRepo.Root, repo.Root, "linked worktree must resolve to the main repo root")

	hooks, err := loadRemoteHooksForPath(worktreeDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(mainRepo.Root, ".agent-factory/hooks/launch.sh"), hooks.LaunchCmd,
		"hooks must resolve against the main repo root, not the worktree")
	assert.NotContains(t, hooks.LaunchCmd, worktreeDir)
}

// TestE2EBackendResolutionWithInRepoConfig verifies that loadRemoteHooksForPath
// reads the in-repo .agent-factory/config.json (#800) and that it shadows the
// legacy per-repo location.
func TestE2EBackendResolutionWithInRepoConfig(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@inrepo.com")
	runGit(t, repoDir, "config", "--local", "user.name", "InRepo Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "f.txt")
	runGit(t, repoDir, "commit", "-m", "init")

	// In-repo remote_hooks alone resolve the remote backend.
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, config.InRepoConfigDirName), 0755))
	require.NoError(t, os.WriteFile(config.InRepoConfigPath(repoDir),
		[]byte(`{"remote_hooks": {"launch_cmd": "/bin/echo in-repo", "delete_cmd": "/bin/echo"}}`), 0644))

	// A conflicting legacy config is shadowed by the in-repo file.
	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoConfig(repo.ID, &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: "/bin/echo legacy",
			DeleteCmd: "/bin/echo",
		},
	}))

	hooks, err := loadRemoteHooksForPath(repoDir)
	require.NoError(t, err)
	assert.Equal(t, "/bin/echo in-repo", hooks.LaunchCmd)
}
