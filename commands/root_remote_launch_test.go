package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// W9.7: against a remote target the TUI opens in registry mode (no local repo,
// even from inside a git checkout) and never starts a local daemon.
func TestRemoteLaunchIsRegistryModeAndSpawnsNoLocalDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("auto_update = false\n"), 0600))
	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	t.Chdir(repo)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")

	oldRun, oldTasks := runLaunchApp, launchEnsureDaemonForTasks
	t.Cleanup(func() { runLaunchApp = oldRun; launchEnsureDaemonForTasks = oldTasks })
	var gotRepo *config.RepoContext
	mounted, spawned := false, false
	runLaunchApp = func(_ context.Context, _, _ string, r *config.RepoContext) error {
		mounted, gotRepo = true, r
		return nil
	}
	launchEnsureDaemonForTasks = func() { spawned = true }

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.True(t, mounted)
	require.Nil(t, gotRepo, "a remote launch is scoped by the target's registry, not this machine's cwd")
	require.False(t, spawned, "a remote launch must not start a local daemon")

	// Control: the same launch with no remote target resolves the cwd repo and
	// ensures the daemon, so the assertions above are not vacuous.
	t.Setenv("AF_DAEMON_URL", "")
	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.NotNil(t, gotRepo)
}
