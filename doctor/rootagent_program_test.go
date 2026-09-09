package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
			Title:          "Root", // reserved-root identity is intentionally case-insensitive
			Program:        "claude",
			RuntimeProgram: "claude",
			Liveness:       session.LiveReady,
			Worktree:       session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath},
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
			Title:          session.RootSessionTitle,
			Program:        "codex",
			RuntimeProgram: "codex",
			Liveness:       session.LiveReady,
			Worktree:       session.GitWorktreeData{RepoPath: barePath, WorktreePath: worktreePath},
		}}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusPass, check.Status)
	require.Contains(t, check.Detail, "match the configured command")
}

func TestRootAgentDefaultProfileMatchesChainedLaunchOverrides(t *testing.T) {
	testguard.IsolateTmux(t)
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())

	body := "schema_version = 1\n[program_overrides]\nclaude = 'codex'\ncodex = '/opt/codex'\n" +
		"[root_agents]\n\"" + repoPath + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	opts.daemonHealth = rootAgentDoctorHealth
	opts.sessionInventory = rootAgentInventory(repoPath, "/opt/codex")

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

func TestRootAgentProgramInspectionBoundsRepositoryProbes(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	body := "schema_version = 1\n[root_agents]\n\"" + repoPath + "\" = { program = \"codex\" }\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = rootAgentInventory(repoPath, "claude")

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nsleep 4\nexec %q \"$@\"\n", realGit)
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	report := runRootAgentProgramCheck(t, opts, cfg)
	require.Less(t, time.Since(started), 8*time.Second, "repository inspection exceeded its probe budget")
	check := findCheck(t, report, "root agent program")
	require.Contains(t, check.Detail, "context deadline exceeded")
	require.Contains(t, report.Incomplete, "root agent program")
}

func TestRootAgentUnreadablePersonalConfigIsIncomplete(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	project, err := config.RegisterProject(repoPath)
	require.NoError(t, err)
	projectConfig, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(projectConfig, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("schema_version = 1\n[root_agent]\nenabled = true\n"), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = rootAgentInventory(repoPath, "claude")

	report := runRootAgentProgramCheck(t, opts, cfg)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, "personal config")
	require.Contains(t, report.Incomplete, "root agent program")
}

func TestRootAgentProgramInspectionDoesNotRecordInRepoLoad(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	require.NoError(t, os.MkdirAll(filepath.Dir(config.InRepoTomlConfigPath(repoPath)), 0o755))
	require.NoError(t, os.WriteFile(config.InRepoTomlConfigPath(repoPath),
		[]byte("post_worktree_commands = ['true']\n"), 0o600))
	body := "schema_version = 1\n[root_agents]\n\"" + repoPath + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	_, err := config.LoadConfig()
	require.NoError(t, err)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	opts.sessionInventory = rootAgentInventory(repoPath, "claude --dangerously-skip-permissions")
	opts.daemonHealth = rootAgentDoctorHealth

	stateDir := filepath.Join(opts.ConfigDir, "repos", repo.ID)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, "sentinel"), []byte("unchanged\n"), 0o600))
	before := directoryBytes(t, stateDir)
	_, err = Run(opts)
	require.NoError(t, err)
	require.Equal(t, before, directoryBytes(t, stateDir), "doctor must leave per-repository state byte-identical")
}

func TestRootAgentStartupUnknownIsIncomplete(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	body := "schema_version = 1\n[root_agents]\n\"" + repoPath + "\" = { program = \"codex\" }\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		instances, inventoryErr := rootAgentInventory(repoPath, "codex")()
		instances[0].StartupStateUnknown = true
		return instances, inventoryErr
	}

	report := runRootAgentProgramCheck(t, opts, cfg)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, "startup state is unknown")
	require.Contains(t, report.Incomplete, "root agent program")
}

func TestRootAgentDisabledProfileWithLiveSessionWarns(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("schema_version = 1\n"), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = rootAgentInventory(repoPath, "claude")

	report := runRootAgentProgramCheck(t, opts, cfg)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, "disabled")
	require.Contains(t, check.Remediation, "kill the root")
	require.True(t, check.Problem)
}

func TestRootAgentPendingCreateIsIncomplete(t *testing.T) {
	opts := testOptions(t, false)
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repoPath).Run())
	body := "schema_version = 1\n[root_agents]\n\"" + repoPath + "\" = { program = \"codex\" }\n"
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName), []byte(body), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		instances, inventoryErr := rootAgentInventory(repoPath, "codex")()
		instances[0].InFlightOp = session.OpCreating
		return instances, inventoryErr
	}

	report := runRootAgentProgramCheck(t, opts, cfg)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, "in-flight")
	require.Contains(t, report.Incomplete, "root agent program")
}

func TestRootAgentMissingRepositoryPathIsIncomplete(t *testing.T) {
	opts := testOptions(t, false)
	require.NoError(t, os.WriteFile(filepath.Join(opts.ConfigDir, config.TomlConfigFileName),
		[]byte("schema_version = 1\n"), 0o600))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	opts.sessionInventory = func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: session.RootSessionTitle, Program: "claude", Liveness: session.LiveReady,
		}}, nil
	}

	report := runRootAgentProgramCheck(t, opts, cfg)
	check := findCheck(t, report, "root agent program")
	require.Equal(t, StatusWarn, check.Status)
	require.Contains(t, check.Detail, "repository path is missing")
	require.Contains(t, report.Incomplete, "root agent program")
}

func rootAgentInventory(repoPath, program string) func() ([]session.InstanceData, error) {
	return func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: session.RootSessionTitle, Program: program, RuntimeProgram: program, Liveness: session.LiveReady,
			Path: repoPath, Worktree: session.GitWorktreeData{RepoPath: repoPath, WorktreePath: repoPath},
		}}, nil
	}
}

func runRootAgentProgramCheck(t *testing.T, opts Options, cfg *config.Config) *Report {
	t.Helper()
	ctx, err := newScanContext(opts)
	require.NoError(t, err)
	report := &Report{}
	checkRootAgentPrograms(ctx, report, cfg)
	return report
}

func directoryBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	contents := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, readErr)
		contents[entry.Name()] = string(data)
	}
	return contents
}

func rootAgentDoctorHealth() daemon.HealthStatus {
	return daemon.HealthStatus{
		SocketPath: "test-daemon.sock",
		BootConfig: &daemon.DaemonBootConfig{ListenAddr: "127.0.0.1:8443"},
	}
}
