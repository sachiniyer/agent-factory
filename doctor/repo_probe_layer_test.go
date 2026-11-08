package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// ---------------------------------------------------------------------------
// repoHeader / checkWorktreeMode: the repo header and worktree-mode row that
// checkConfigAndStorage emits BEFORE checkAgentBinaries runs.
// ---------------------------------------------------------------------------

// repoHeaderValue returns the value of the "repo" header line, or "" if absent.
func repoHeaderValue(r *Report) string {
	for _, h := range r.Header {
		if h.Label == "repo" {
			return h.Value
		}
	}
	return ""
}

// TestRepoHeader_AnsweredGitFailure_ClassifiesAsNotInspectable pins the #6995
// review fix: when the repo probe fails with an answered git refusal (NOT
// ErrNotGitRepository), the repo header and the worktree-mode row must NOT
// report "not inside a git repository" / "run from inside a git repository" —
// that contradicts the agent-config finding checkAgentBinaries emits for the
// same probe and sends a user with dubious ownership toward the wrong remedy.
// The header says "repo not inspectable" and the worktree row stays advisory.
func TestRepoHeader_AnsweredGitFailure_ClassifiesAsNotInspectable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	restore := answeredFailureRepo(t, repo)
	defer restore()

	_, repoErr := currentRepoRoot()
	require.Error(t, repoErr)
	require.False(t, errors.Is(repoErr, config.ErrNotGitRepository),
		"fixture: the answered refusal must not be the not-a-repo verdict")

	require.Equal(t, "n/a (repo not inspectable)", repoHeader("", repoErr, ""),
		"an answered refusal is not a not-a-repo verdict; the header must say the repo was not inspectable")

	rep := &Report{}
	checkWorktreeMode(&scanContext{opts: Options{ConfigDir: t.TempDir()}}, rep, "", repoErr, "")
	row := findCheck(t, rep, "worktrees")
	require.Equal(t, StatusWarn, row.Status, "the worktree row stays advisory — the actionable finding is checkAgentBinaries'")
	require.Contains(t, row.Detail, "repo not inspectable",
		"the worktree row must say the repo was not inspectable, not that the doctor is not inside a repository")
	require.NotContains(t, row.Remediation, "inside a git repository",
		"advising the user to run from inside a repository contradicts an answered refusal the doctor already tried")
	require.False(t, row.Problem, "the worktree row must not double-count the probe's actionable finding")
}

// TestRepoHeader_GenuineNonRepo_StillClassifiesAsNotInsideARepo is the regression
// lock for the genuine non-repo case: a plain directory with no .git must keep
// the "not inside a git repository" header and "run from inside a git
// repository" advice. The distinction the fix introduces must not soften the
// true non-repo verdict.
func TestRepoHeader_GenuineNonRepo_StillClassifiesAsNotInsideARepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	binDir := t.TempDir()
	linkRealGitInto(t, binDir)
	t.Setenv("PATH", binDir)
	chdir(t, t.TempDir()) // a directory that is not a git repository

	_, repoErr := currentRepoRoot()
	require.Error(t, repoErr)
	require.True(t, errors.Is(repoErr, config.ErrNotGitRepository),
		"fixture: a plain directory must be the genuine not-a-repo verdict")
	require.Equal(t, "n/a (not inside a git repository)", repoHeader("", repoErr, ""),
		"a genuine non-repo must still be reported as not inside a git repository")

	rep := &Report{}
	checkWorktreeMode(&scanContext{opts: Options{ConfigDir: t.TempDir()}}, rep, "", repoErr, "")
	row := findCheck(t, rep, "worktrees")
	require.Contains(t, row.Remediation, "inside a git repository",
		"a genuine non-repo must still advise running from inside a git repository")
}

// ---------------------------------------------------------------------------
// Shared repo probe (default profile): the repo header, worktree-mode row, and
// agent-config row must observe ONE repo probe so a flaky git cannot make them
// disagree (#6995 review).
// ---------------------------------------------------------------------------

// installFlakyProbeGit installs a git shim first on PATH that fails its FIRST
// invocation (kills itself → an unanswered probe) and delegates to the real git
// thereafter. The real git must already have been used for repo setup before
// this is installed. It returns the path to a counter file whose content is the
// number of git invocations the shim has fielded, so a test can assert how many
// probes actually ran.
func installFlakyProbeGit(t *testing.T) (counter string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err, "git must be on PATH for the test to run")
	binDir := t.TempDir()
	counter = filepath.Join(binDir, "counter")
	require.NoError(t, os.WriteFile(counter, []byte("0"), 0o644))
	shim := "#!/bin/sh\n" +
		"n=$(cat " + counter + ")\n" +
		"n=$((n+1))\n" +
		"echo $n > " + counter + "\n" +
		"if [ \"$n\" -eq 1 ]; then kill -9 $$; fi\n" +
		"exec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755))
	t.Setenv("PATH", binDir)
	return counter
}

// TestScanContext_RepoProbe_Memoizes pins the memoization that lets the default
// profile share one probe: two calls on the same scanContext run git exactly
// once and return the same result. Without this, checkConfigAndStorage and
// checkAgentBinaries would each probe independently and a flaky git could make
// their verdicts disagree (#6995 review).
func TestScanContext_RepoProbe_Memoizes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	counter := installFlakyProbeGit(t)

	ctx := &scanContext{opts: Options{ConfigDir: t.TempDir()}}
	r1, err1 := ctx.repoProbe()
	r2, err2 := ctx.repoProbe()
	require.Same(t, r1, r2, "the memoized probe must return the same *RepoContext")
	require.Equal(t, err1, err2)
	require.Error(t, err1, "the first (flaky-failed) probe is the one both calls see")
	require.Nil(t, r1, "a failed probe returns a nil RepoContext")
	require.True(t, config.RepoProbeUnanswered(err1),
		"fixture: the killed first probe is an unanswered probe")
	require.Equal(t, "1", strings.TrimSpace(string(mustReadFile(t, counter))),
		"exactly one git invocation must run across both calls")
}

// TestDefaultProfile_RepoHeaderAndAgentChecks_ShareProbe pins the end-to-end
// sharing fix (#6995 review): when the repo probe is flaky — the first call
// fails and a later one would succeed — the repo header and the agent-config
// row must NOT diverge. Before the fix, checkConfigAndStorage saw the failure
// (header "repo not inspectable", worktree skipped) while checkAgentBinaries'
// second probe succeeded and emitted PASS rows, so the run exited 0 having
// skipped the worktree check. Sharing one memoized probe makes both see the
// same first failure.
func TestDefaultProfile_RepoHeaderAndAgentChecks_ShareProbe(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	chdir(t, repo)
	counter := installFlakyProbeGit(t)

	// A present global-layer binary so the OLD bug's second probe (which would
	// succeed) would have emitted PASS rows — the only reason agents stays
	// "not inspectable" here is the shared first-probe failure.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: filepath.Join(binDir, "claude")}

	ctx := &scanContext{opts: Options{ConfigDir: t.TempDir()}}
	rep := &Report{}
	checkConfigAndStorage(ctx, rep)
	checkAgentBinariesImpl(ctx, cfg, rep)

	require.Contains(t, repoHeaderValue(rep), "repo not inspectable",
		"the first probe failed; the repo header must say the repo was not inspectable")
	require.Contains(t, agentsHeaderValue(rep), "repo not inspectable",
		"the agent checks must see the SAME (failed) probe, not a fresh successful one")
	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status, "the shared failed probe drives an agent-config FAIL")
	require.True(t, row.Problem, "the agent-config FAIL must drive the exit code")
	require.True(t, noGlobalLayerPassRows(rep),
		"the shared failed probe must not emit global-layer PASS rows")
	require.Equal(t, "1", strings.TrimSpace(string(mustReadFile(t, counter))),
		"exactly one git invocation must run across the header and the agent checks — the probe is shared")
}

// ---------------------------------------------------------------------------
// cwd-resolution errors: a getwd failure (e.g. the cwd was removed mid-run) is
// neither "outside a repository" nor a git refusal, so the not-inspectable
// finding must not label it "git's error" or advise safe.directory (#6995
// review).
// ---------------------------------------------------------------------------

// TestCheckAgentBinaries_CwdUnreadable_NotLabeledAsGitError pins the
// default-profile fix: when os.Getwd fails before git is asked anything, the
// agent-config finding must name the cwd failure and advise moving to an
// accessible directory — not "git's error" + safe.directory, which cannot fix a
// missing cwd. Before the fix, repoNotInspectableDetail formatted repoErr with
// %v (yielding only the getwd path error) and recommended safe.directory.
func TestCheckAgentBinaries_CwdUnreadable_NotLabeledAsGitError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// chdir into a directory, then remove it so os.Getwd fails for the
	// checkAgentBinaries probe. The chdir helper's cleanup restores the
	// original (still-extant) cwd afterward.
	gone := t.TempDir()
	chdir(t, gone)
	require.NoError(t, os.Remove(gone))

	cfg := config.DefaultConfig()
	cfg.DefaultProgram = tmux.ProgramClaude
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"a cwd-resolution failure must produce an agent-config FAIL, not a silent global-layer PASS")
	require.True(t, row.Problem, "the cwd failure must drive the exit code")
	require.Contains(t, row.Detail, "could not determine the current directory",
		"the finding must name the cwd failure, not attribute it to git")
	require.NotContains(t, row.Detail, "git's error",
		"a getwd failure is not a git error and must not be labeled as one")
	require.Contains(t, row.Remediation, "move to a directory",
		"the remedy is to leave the gone directory, not to fix git")
	require.NotContains(t, row.Remediation, "safe.directory",
		"safe.directory cannot fix a missing cwd and must not be suggested for it")
}

// mustReadFile reads path or fails the test.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
