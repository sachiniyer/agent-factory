package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestRootAgentProgramDriftNamesBothCommandsAndRemedy is issue #4087's
// diagnostic regression: doctor reads the current config from disk and compares
// it with the live root reported by the daemon, so the silent no-op has a place
// the operator can inspect even when the daemon's frozen profile predates an
// edit.
func TestRootAgentProgramDriftNamesBothCommandsAndRemedy(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())

	body := "schema_version = 1\n[root_agents]\n\"" + repoPath + "\" = { program = \"codex\" }\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	opts.daemonHealth = func() daemon.HealthStatus {
		return daemon.HealthStatus{
			SocketPath: "test-daemon.sock",
			BootConfig: &daemon.DaemonBootConfig{
				ListenAddr: "127.0.0.1:8443",
			},
		}
	}
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    "Root", // reserved-root identity is intentionally case-insensitive
			Program:  "claude",
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath},
		}}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, repoPath)
	require.Contains(t, check.Detail, `configured command "codex"`)
	require.Contains(t, check.Detail, `running command "claude"`)
	require.Contains(t, check.Remediation, "kill the root, then restart the daemon")
	require.True(t, check.Problem)
}

func TestRootAgentProgramDriftResolvesFromLiveWorktreePath(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	root := t.TempDir()
	seedPath := filepath.Join(root, "seed")
	barePath := filepath.Join(root, "identity.git")
	worktreePath := filepath.Join(root, "checkout")
	require.NoError(t, exec.Command("git", "init", seedPath).Run())
	require.NoError(t, exec.Command("git", "-C", seedPath, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", seedPath, "config", "user.name", "Test User").Run())
	require.NoError(t, os.MkdirAll(filepath.Join(seedPath, config.InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedPath, config.InRepoConfigDirName, config.TomlConfigFileName),
		[]byte("[program_overrides]\nclaude = 'codex'\n"), 0o600))
	require.NoError(t, exec.Command("git", "-C", seedPath, "add", ".").Run())
	require.NoError(t, exec.Command("git", "-C", seedPath, "commit", "-m", "init").Run())
	require.NoError(t, exec.Command("git", "clone", "--bare", seedPath, barePath).Run())
	require.NoError(t, exec.Command("git", "--git-dir", barePath, "worktree", "add", worktreePath).Run())

	body := "schema_version = 1\n[root_agents]\n\"" + worktreePath + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	opts.daemonHealth = rootAgentDoctorHealth
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    session.RootSessionTitle,
			Program:  "codex",
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{RepoPath: barePath, WorktreePath: worktreePath},
		}}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusPass, check.Status)
	require.Contains(t, check.Detail, "match the configured command")
}

func TestRootAgentProgramResolutionFailureIsIncomplete(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(
		filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte("schema_version = 1\n"), 0o600))
	opts.daemonHealth = rootAgentDoctorHealth
	missing := filepath.Join(t.TempDir(), "missing-checkout")
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: session.RootSessionTitle, Program: "claude", Path: missing, Liveness: session.LiveReady,
		}}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, missing)
	require.Contains(t, check.Detail, "could not resolve")
	require.Contains(t, report.Incomplete, "root agent program")
}

func rootAgentDoctorHealth() daemon.HealthStatus {
	return daemon.HealthStatus{
		SocketPath: "test-daemon.sock",
		BootConfig: &daemon.DaemonBootConfig{ListenAddr: "127.0.0.1:8443"},
	}
}
