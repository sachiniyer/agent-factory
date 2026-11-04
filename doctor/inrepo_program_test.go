package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// writeInRepoConfig writes a .agent-factory/config.toml inside repo with the
// given body, creating the directory if needed.
func writeInRepoConfig(t *testing.T, repo, body string) {
	t.Helper()
	dir := filepath.Join(repo, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(body), 0o644))
}

// globalConfigWithoutOverrides returns a full default config with no
// program_overrides, so the in-repo layer is the only place an override can
// come from. DefaultConfig may auto-detect a claude override from PATH; clearing
// ProgramOverrides after the call removes it so the test is deterministic.
func globalConfigWithoutOverrides() *config.Config {
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = nil
	return cfg
}

// TestCheckAgentBinaries_InRepoOverride_MissingBinary_FailsNotWarn is the core
// guarantee from the bug report: a repo with
// program_overrides.claude = "./bin/claude" where ./bin/claude does NOT exist
// must produce a FAIL (actionable, exit-code impact), not a WARN. Before the
// fix the sweep resolved the command from the global layer only — which had no
// claude override — so `configured` was false and a missing binary was demoted
// to a non-actionable WARN (exit 0). The fix resolves the effective config from
// the in-repo layer, so `configured` is true and the missing binary is a FAIL.
func TestCheckAgentBinaries_InRepoOverride_MissingBinary_FailsNotWarn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status,
		"a missing in-repo override is configured → FAIL, not WARN")
	require.True(t, row.Problem, "a configured agent's missing binary must be actionable")
	require.Contains(t, row.Detail, "./bin/claude",
		"the command checked must be the in-repo override, not the bare name")
	require.Contains(t, agentsHeaderValue(rep), "claude=missing")
}

// TestCheckAgentBinaries_InRepoOverride_PresentBinary_Passes verifies the
// happy path: a repo with program_overrides.claude = "bin/claude" where
// bin/claude exists at the repo root must PASS, resolving to the in-repo
// binary — not the bare name from the global layer.
func TestCheckAgentBinaries_InRepoOverride_PresentBinary_Passes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"the in-repo override resolves to a present binary → PASS")
	require.False(t, row.Problem)
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row),
		"the resolved path must be the in-repo binary, not a PATH lookup of the bare name")
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
}

// TestCheckAgentBinaries_InRepoOverride_FalsePassEliminated is the exact
// "false PASS" scenario from the bug report: a repo with
// program_overrides.claude = "./bin/claude" (missing) AND a fake `claude` on
// PATH. Before the fix the sweep checked the bare name `claude` (from the
// global layer, which had no override) and found it on PATH → PASS. The fix
// checks `./bin/claude` (from the in-repo layer) and correctly FAILs.
func TestCheckAgentBinaries_InRepoOverride_FalsePassEliminated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	// A fake `claude` on PATH — the binary the bug-report's false PASS checked.
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status,
		"the doctor must check the in-repo override ./bin/claude (missing), not the bare name claude (on PATH)")
	require.True(t, row.Problem, "the missing in-repo override is configured → actionable")
	require.Contains(t, row.Detail, "./bin/claude",
		"the command checked must be the in-repo override")
	require.NotContains(t, row.Detail, binDir,
		"the PATH binary must not appear in the detail — a different binary was checked")
	require.Contains(t, agentsHeaderValue(rep), "claude=missing")
}

// TestCheckAgentBinaries_InRepoOverride_WinsOverGlobal verifies in-repo wins
// precedence: when the global layer has program_overrides.claude = "/global/claude"
// (missing) and the in-repo layer has program_overrides.claude = "bin/claude"
// (present at the repo root), the doctor must check the in-repo value and PASS,
// not the global value and FAIL.
func TestCheckAgentBinaries_InRepoOverride_WinsOverGlobal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"bin/claude\"\n")
	chdir(t, repo)

	// Global config has a DIFFERENT (missing) override for claude.
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "/global/claude"}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"the in-repo override (present) wins over the global override (missing)")
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row),
		"the resolved path must be the in-repo binary, not the global override")
	require.NotContains(t, row.Detail, "/global/claude",
		"the global override must not be the command checked")
}

// TestCheckAgentBinaries_NoInRepoConfig_FallsBackToGlobal verifies that outside
// a repo with an in-repo config (or inside a repo with no in-repo config), the
// sweep falls back to the global layer — the only layer sessions could launch
// under there. A global override pointing at a present binary must PASS.
func TestCheckAgentBinaries_NoInRepoConfig_FallsBackToGlobal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	// No .agent-factory/config.toml in the repo.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)

	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"with no in-repo config the global override is used")
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
}

// TestCheckAgentPrograms_InRepoOverride_PresentBinary_Passes is the setup-path
// (`af doctor --setup`) twin: a repo with program_overrides.claude = "bin/claude"
// (present at the repo root) and no global override must PASS the setup sweep.
// Before the fix the setup path resolved the default_program from the global
// layer only, so it checked the bare name (or the global override) instead of
// the in-repo override.
func TestCheckAgentPrograms_InRepoOverride_PresentBinary_Passes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount(), "agent-program findings: %+v", rep.Findings)
	row := findCheck(t, rep, "default_program")
	require.Equal(t, StatusPass, row.Status,
		"the in-repo override resolves to a present binary → PASS")
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row),
		"the resolved path must be the in-repo binary")
	override := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, override.Status)
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, override))
}

// TestCheckAgentPrograms_InRepoOverride_MissingBinary_Actionable is the
// setup-path severity guarantee: a repo with
// program_overrides.claude = "./bin/claude" where ./bin/claude does NOT exist
// must produce an actionable finding for the in-repo command, not silently pass
// by checking the bare name from the global layer.
func TestCheckAgentPrograms_InRepoOverride_MissingBinary_Actionable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings, "a missing in-repo override must produce a finding")
	// The finding must name the in-repo command, not the bare name.
	var found bool
	for _, f := range findings {
		if f.Check == "agent-program" {
			require.Contains(t, f.Detail, "./bin/claude",
				"the finding must name the in-repo override command")
			found = true
		}
	}
	require.True(t, found, "an agent-program finding naming the in-repo command must exist")
}

// TestDoctor_InRepoOverride_EndToEnd verifies the full `af doctor` run resolves
// the effective config from the in-repo layer: a repo with
// program_overrides.claude = "bin/claude" (present at the repo root) and no
// global override must PASS the environment sweep, and the agents header must
// report claude=present.
func TestDoctor_InRepoOverride_EndToEnd(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Global config: default_program = "claude", no program_overrides.
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o644))

	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"bin/claude\"\n")
	chdir(t, repo)

	report, err := Run(Options{
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
		inRepoUnknownLeaves: func() []config.InRepoUnknownLeaf { return nil },
	})
	require.NoError(t, err)
	row := findCheckByName(t, report, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"the in-repo override resolves to a present binary → PASS")
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row),
		"the resolved path must be the in-repo binary")
	require.Contains(t, agentsHeaderValue(report), "claude=present")
}

// TestDoctor_InRepoOverride_MissingBinary_EndToEnd verifies the full `af doctor`
// run reports a FAIL (not WARN) when an in-repo override points at a missing
// binary, and that the exit-code-impacting UnresolvedCount reflects it.
func TestDoctor_InRepoOverride_MissingBinary_EndToEnd(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	binDir := t.TempDir()
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o644))

	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	report, err := Run(Options{
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
		inRepoUnknownLeaves: func() []config.InRepoUnknownLeaf { return nil },
	})
	require.NoError(t, err)
	row := findCheckByName(t, report, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status,
		"a missing in-repo override is configured → FAIL, not WARN")
	require.True(t, row.Problem, "the missing in-repo override must be actionable")
	require.Contains(t, row.Detail, "./bin/claude")
	require.NotZero(t, report.UnresolvedCount(),
		"a FAIL row must drive the exit code")
}

// TestCheckAgentBinaries_MalformedInRepoConfig_FailsNotSilentGlobal verifies
// that when the in-repo config exists but cannot be resolved (a TOML syntax
// error), the sweep reports an actionable config failure instead of silently
// falling back to the global layer. Session creation refuses the same repo
// with this error (#5801), so a silent global fallback would let `af doctor`
// PASS for a repo `af` itself would not launch a session in.
func TestCheckAgentBinaries_MalformedInRepoConfig_FailsNotSilentGlobal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	// Invalid TOML: an unclosed string table value. The in-repo loader turns
	// this into a parse error that ResolveConfigForRepoInspectionWithGlobal
	// surfaces.
	writeInRepoConfig(t, repo, "backend = \"docker\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"an unresolvable in-repo config must FAIL, not silently fall back to global")
	require.True(t, row.Problem, "the unresolved in-repo config must be actionable")
	require.Contains(t, row.Detail, "could not resolve the in-repo config")
	// No per-agent binary rows ran: the session would refuse to launch, so the
	// doctor must not pretend to have checked a command.
	for _, c := range rep.Checks {
		require.NotEqual(t, tmux.ProgramClaude, c.Name,
			"no per-agent binary row should run when the in-repo config is unresolvable")
	}
	require.Contains(t, agentsHeaderValue(rep), "repo config unresolved")
}

// TestCheckAgentBinaries_RemoteBackend_SkipsHostChecks verifies that a repo
// whose in-repo config selects a non-local backend (docker) does not check the
// daemon host for the agent binary: the session runs in a container, so the
// host's binary is not a prerequisite. Mirrors daemon.createNeedsDaemonHostPrereqs.
func TestCheckAgentBinaries_RemoteBackend_SkipsHostChecks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	// No claude binary exists anywhere — on a docker backend that is fine.
	writeInRepoConfig(t, repo, "backend = \"docker\"\n[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	require.Contains(t, agentsHeaderValue(rep), "skipped (repo backend: docker)",
		"the agents header must record that host checks were skipped for a remote backend")
	row := findCheckByName(t, rep, "agents")
	require.Equal(t, StatusInfo, row.Status,
		"a remote-backend skip is an observation, not a pass/fail")
	require.NotZero(t, row.Detail, "docker")
	// No per-agent binary rows: the daemon host is not checked.
	for _, c := range rep.Checks {
		require.NotEqual(t, tmux.ProgramClaude, c.Name,
			"no per-agent binary row should run on a remote backend")
	}
	require.Zero(t, rep.UnresolvedCount(),
		"a remote-backend skip must not drive the exit code")
}

// TestCheckAgentPrograms_RemoteBackend_SkipsHostChecks is the setup-path twin
// of TestCheckAgentBinaries_RemoteBackend_SkipsHostChecks: a repo on a docker
// backend must not report the daemon host's missing agent binary as a setup
// finding.
func TestCheckAgentPrograms_RemoteBackend_SkipsHostChecks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "backend = \"docker\"\ndefault_program = \"claude\"\n[program_overrides]\nclaude = \"./bin/claude\"\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount(),
		"a remote-backend skip must not produce an actionable finding: %+v", rep.Findings)
	row := findCheck(t, rep, "agent-program")
	require.Equal(t, StatusPass, row.Status,
		"a remote-backend skip is recorded as a non-actionable pass row")
	require.Contains(t, row.Detail, "docker")
}

// TestCheckAgentPrograms_MalformedInRepoConfig_Actionable is the setup-path twin
// of TestCheckAgentBinaries_MalformedInRepoConfig_FailsNotSilentGlobal: an
// unresolvable in-repo config must produce an actionable agent-program finding
// rather than silently checking the global-layer command.
func TestCheckAgentPrograms_MalformedInRepoConfig_Actionable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"claude\n")
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings, "an unresolvable in-repo config must produce a finding")
	require.Contains(t, findings[0].Detail, "could not resolve the in-repo config")
	require.True(t, rep.UnresolvedCount() > 0,
		"an unresolvable in-repo config must drive the exit code")
}
