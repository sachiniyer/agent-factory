package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// installUnansweredProbeGit installs a git shim first on PATH that kills itself
// with signal 9, so every git invocation produces an unanswered probe
// (config.ErrRepoProbeUnanswered). The real git must already have been used for
// repo setup before this is installed — setupDoctorRepo and commitRepoFile run
// real git, so call those first.
//
// Mirrors config/repo_probe_test.go's installFakeGit pattern: a signalled death
// has a negative exit code, so probeWentUnanswered classifies it as unanswered,
// and RepoFromPath wraps the error with ErrRepoProbeUnanswered. The doctor's
// agent checks must then classify the repo probe as "not inspectable" rather than
// silently falling back to the global layer.
func installUnansweredProbeGit(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"),
		[]byte("#!/bin/sh\nkill -9 $$\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// noGlobalLayerPassRows reports whether the report contains no PASS row whose
// Name is a global-layer agent-program field — "default_program",
// "program_overrides.<agent>", or any of tmux.SupportedPrograms. When the repo
// probe fails for a non-ErrNotGitRepository reason, the doctor must not emit
// PASS rows for a config layer the repo's sessions will not use.
func noGlobalLayerPassRows(r *Report) bool {
	for _, c := range r.Checks {
		if c.Status != StatusPass {
			continue
		}
		if c.Name == "default_program" {
			return false
		}
		if len(c.Name) > len("program_overrides.") && c.Name[:len("program_overrides.")] == "program_overrides." {
			return false
		}
		for _, agent := range tmux.SupportedPrograms {
			if c.Name == agent {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// checkAgentPrograms (af doctor --setup): the setup-path agent-program checks.
// ---------------------------------------------------------------------------

// TestCheckAgentPrograms_AnsweredGitFailure_NotSilentGlobalLayer is the
// setup-path guarantee: when the repo probe fails with an answered git refusal
// (dubious ownership, an invalid .git — NOT ErrNotGitRepository), the doctor
// must emit an actionable finding that the repo could not be inspected and must
// NOT emit PASS rows for the global layer's programs. Before the fix, the
// doctor silently fell back to the global layer and emitted false PASSes for
// programs the repo's sessions would never launch — contradicting the
// ErrNotGitRepository contract in config/repo.go.
func TestCheckAgentPrograms_AnsweredGitFailure_NotSilentGlobalLayer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	// In-repo layer: a different program than the global layer, so a
	// global-layer PASS would be checking the wrong command.
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	// Global layer: a present binary so the old bug would have PASSed.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings, "an answered git refusal must produce an agent-program finding, not a silent global-layer PASS")
	require.True(t, findings[0].Actionable,
		"the repo-not-inspectable finding must be actionable (exit-code impact)")
	require.Contains(t, findings[0].Detail, "could not inspect the repository",
		"the finding must say the repo could not be inspected, not that it is not a repository")
	require.NotContains(t, findings[0].Detail, "is not inside a git repository",
		"an answered refusal is not a not-a-repository verdict")
	require.NotZero(t, rep.UnresolvedCount(),
		"an actionable finding must drive the exit code")
	require.True(t, noGlobalLayerPassRows(rep),
		"no PASS rows for global-layer programs should appear when the repo could not be inspected")
}

// TestCheckAgentPrograms_UnansweredProbe_NotSilentGlobalLayer is the
// setup-path guarantee for the unanswered-probe case: when the repo probe never
// answered (git was killed, could not be started, or was abandoned mid-read),
// the doctor must emit an actionable finding using the resolver's unanswered-
// probe claim and must NOT emit PASS rows for the global layer's programs.
// Before the fix, this case was fully silent — the doctor could exit 0 with
// PASS rows for a config layer the repo's sessions would never launch.
func TestCheckAgentPrograms_UnansweredProbe_NotSilentGlobalLayer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)

	// Global layer: a present binary so the old bug would have PASSed.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}

	installUnansweredProbeGit(t)
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings, "an unanswered probe must produce an agent-program finding, not a silent global-layer PASS")
	require.True(t, findings[0].Actionable,
		"the repo-not-inspectable finding must be actionable (exit-code impact)")
	require.Contains(t, findings[0].Detail, "could not be checked",
		"the finding must use the unanswered-probe claim, not a not-a-repository verdict")
	require.Contains(t, findings[0].Detail, "git never answered the probe",
		"the finding must say the probe never answered")
	require.NotContains(t, findings[0].Detail, "is not inside a git repository",
		"an unanswered probe is not a not-a-repository verdict")
	require.NotZero(t, rep.UnresolvedCount(),
		"an actionable finding must drive the exit code")
	require.True(t, noGlobalLayerPassRows(rep),
		"no PASS rows for global-layer programs should appear when the repo could not be inspected")
}

// TestCheckAgentPrograms_GenuineNonRepo_StillFallsBackToGlobal is the regression
// lock: a genuine non-repo (ErrNotGitRepository) must still fall back to the
// global layer — the fix is a distinction (answered refusal / unanswered probe
// vs. genuine non-repo), not a softening. The global layer's programs must be
// checked and PASS when present, and no "repo not inspectable" finding should
// appear.
func TestCheckAgentPrograms_GenuineNonRepo_StillFallsBackToGlobal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	chdir(t, t.TempDir()) // a directory that is not a git repository

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "default_program")
	require.Equal(t, StatusPass, row.Status,
		"a genuine non-repo must still fall back to the global layer (regression)")
	override := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, override.Status,
		"the global override must be checked and PASS for a genuine non-repo")
	findings := findByCheck(rep, "agent-program")
	for _, f := range findings {
		require.NotContains(t, f.Detail, "could not inspect the repository",
			"a genuine non-repo must not produce a repo-not-inspectable finding")
		require.NotContains(t, f.Detail, "could not be checked",
			"a genuine non-repo must not produce an unanswered-probe finding")
	}
}

// ---------------------------------------------------------------------------
// checkAgentBinaries (af doctor): the default-profile agent-binary checks.
// ---------------------------------------------------------------------------

// TestCheckAgentBinaries_AnsweredGitFailure_NotSilentGlobalLayer is the
// default-profile twin of TestCheckAgentPrograms_AnsweredGitFailure: when the
// repo probe fails with an answered git refusal, the doctor must emit an
// actionable agent-config FAIL row and must NOT emit per-agent PASS rows for
// the global layer's programs.
func TestCheckAgentBinaries_AnsweredGitFailure_NotSilentGlobalLayer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"an answered git refusal must produce an agent-config FAIL, not a silent global-layer PASS")
	require.True(t, row.Problem, "the repo-not-inspectable finding must be actionable")
	require.Contains(t, row.Detail, "could not inspect the repository",
		"the finding must say the repo could not be inspected")
	require.NotContains(t, row.Detail, "is not inside a git repository",
		"an answered refusal is not a not-a-repository verdict")
	require.Contains(t, agentsHeaderValue(rep), "repo not inspectable",
		"the agents header must record the repo was not inspectable")
	require.NotZero(t, rep.UnresolvedCount(),
		"an actionable FAIL must drive the exit code")
	require.True(t, noGlobalLayerPassRows(rep),
		"no per-agent PASS rows should appear when the repo could not be inspected")
}

// TestCheckAgentBinaries_UnansweredProbe_NotSilentGlobalLayer is the
// default-profile twin for the unanswered-probe case.
func TestCheckAgentBinaries_UnansweredProbe_NotSilentGlobalLayer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}

	installUnansweredProbeGit(t)
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"an unanswered probe must produce an agent-config FAIL, not a silent global-layer PASS")
	require.True(t, row.Problem, "the repo-not-inspectable finding must be actionable")
	require.Contains(t, row.Detail, "could not be checked",
		"the finding must use the unanswered-probe claim")
	require.Contains(t, row.Detail, "git never answered the probe",
		"the finding must say the probe never answered")
	require.NotContains(t, row.Detail, "is not inside a git repository",
		"an unanswered probe is not a not-a-repository verdict")
	require.Contains(t, agentsHeaderValue(rep), "repo not inspectable",
		"the agents header must record the repo was not inspectable")
	require.NotZero(t, rep.UnresolvedCount(),
		"an actionable FAIL must drive the exit code")
	require.True(t, noGlobalLayerPassRows(rep),
		"no per-agent PASS rows should appear when the repo could not be inspected")
}

// TestCheckAgentBinaries_GenuineNonRepo_StillFallsBackToGlobal is the
// default-profile regression lock: a genuine non-repo must still fall back to
// the global layer.
func TestCheckAgentBinaries_GenuineNonRepo_StillFallsBackToGlobal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	chdir(t, t.TempDir()) // a directory that is not a git repository

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"a genuine non-repo must still fall back to the global layer (regression)")
	require.Contains(t, agentsHeaderValue(rep), "claude=present",
		"the global override must be checked and PASS for a genuine non-repo")
	for _, c := range rep.Checks {
		require.NotEqual(t, "agent-config", c.Name,
			"no agent-config FAIL should appear for a genuine non-repo")
	}
}

// TestCheckAgentPrograms_AnsweredGitFailure_SurfacesGitStderr pins the
// setup-profile stderr-surfacing: when checkAgentPrograms runs in isolation
// (no prior git/git-repo finding), the agent-program finding must carry git's
// own stderr diagnostic, not just "exit status 128" (#6995 review).
func TestCheckAgentPrograms_AnsweredGitFailure_SurfacesGitStderr(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings)
	require.Contains(t, findings[0].Detail, "fatal:",
		"the finding must surface git's own stderr diagnostic, not just the exit status")
	require.NotContains(t, findings[0].Detail, "exit status 128",
		"the bare exit-status text is not git's diagnostic and must not stand in for it")
}

// TestCheckSetup_AnsweredGitFailure_DoesNotDoubleCount pins the double-count
// fix (#6995 review): in the --setup flow, checkGit already records the
// git-repo finding for an answered git refusal, so checkAgentPrograms must
// NOT add a second agent-program finding for the same underlying git failure.
// Before the fix, UnresolvedCount reported two issues for one git error.
func TestCheckSetup_AnsweredGitFailure_DoesNotDoubleCount(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	binDir := t.TempDir()
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")
	linkRealGitInto(t, binDir)
	t.Setenv("PATH", binDir)
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}

	rep := &Report{}
	checkGit(rep)
	checkAgentPrograms(cfg, rep)

	// checkGit's git-repo finding remains the single actionable record of the
	// underlying git failure; checkAgentPrograms must not duplicate it.
	require.NotEmpty(t, findByCheck(rep, "git-repo"),
		"checkGit must record the underlying git failure as a git-repo finding")
	require.Empty(t, findByCheck(rep, "agent-program"),
		"checkAgentPrograms must not add a second agent-program finding for the same git failure (double-count)")
}

// TestCheckAgentBinaries_AnsweredGitFailure_SurfacesGitStderr pins the
// default-profile stderr-surfacing fix (#6995 review): when Git answers with a
// refusal, the agent-config finding must carry Git's own diagnostic line (the
// captured *exec.ExitError.Stderr), not just the bare "exit status 128" that
// %v of an *exec.ExitError produces. Without the extraction the user sees a
// finding labeled "git's error" that does not name the error, leaving them
// unable to pick the matching remedy in the default af doctor profile.
func TestCheckAgentBinaries_AnsweredGitFailure_SurfacesGitStderr(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "default_program = \"codex\"\n[program_overrides]\ncodex = \"bin/definitely-missing\"\n")
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status)
	require.Contains(t, row.Detail, "fatal:",
		"the finding must surface git's own stderr diagnostic, not just the exit status")
	require.NotContains(t, row.Detail, "exit status 128",
		"the bare exit-status text is not git's diagnostic and must not stand in for it")
}
