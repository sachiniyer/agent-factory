package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The bug (#6487): `af doctor` resolved relative program_overrides against its
// own cwd (preflight.CheckCommand's os.Getwd fallback), not against the
// session's launch directory. A relative override like
// `program_overrides.claude = "bin/claude"` flipped between PASS and FAIL
// purely on where the doctor was invoked from. The fix adopts the daemon-side
// #5779 machinery: when the doctor runs inside a repo, the override resolves
// against the repo's workspace via preflight.CheckCommandAt, the same anchor
// the launch uses. Outside any repo the launch directory cannot be determined,
// so today's CheckCommand is kept and any failure is reported with an honest
// message that points the user at the session's repo rather than at installing
// or reinstalling a binary.

// TestCheckAgentPrograms_RelativeOverride_AnchoredToRepoRoot_NotCwd is the
// setup-path (`af doctor --setup`) core guarantee: a relative override that
// exists at <repo>/bin/claude resolves to it when the doctor is run from a
// SUBDIR of the same repo. With the bug, the doctor's cwd-relativity joined
// "bin/claude" against <repo>/subdir and falsely failed; the fix anchors the
// resolution to the repo's workspace, the same anchor the launch uses.
func TestCheckAgentPrograms_RelativeOverride_AnchoredToRepoRoot_NotCwd(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	// Run from a SUBDIR of the repo: the binary lives at the repo root, not
	// under the cwd, so cwd-relativity would resolve to <subdir>/bin/claude
	// (missing) while workspace-relativity (the fix) resolves to
	// <repo>/bin/claude (present).
	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdir(t, subdir)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount(), "agent-program findings: %+v", rep.Findings)
	row := findCheck(t, rep, "default_program")
	require.Equal(t, StatusPass, row.Status, "the relative override resolves against the repo workspace")
	require.Contains(t, row.Detail, filepath.Join(repo, "bin", "claude"),
		"the resolved path must be in the repo, not the cwd")
	override := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, override.Status)
	require.Contains(t, override.Detail, filepath.Join(repo, "bin", "claude"))
}

// TestCheckAgentPrograms_RelativeOverride_OutsideRepo_HonestMessageNotReinstall
// pins the doctor's response when it is invoked outside any git repository: a
// relative override cannot be anchored, and the bug's cwd-resolved resolution
// produced a false "Claude Code is not installed..." failure. The narrowing
// keeps today's check (actionable) but makes the failure message honest — it
// says the command did not resolve from this directory and that a relative
// program_overrides path resolves in each session's workspace, and it never
// sends the reader to install or reinstall a binary.
func TestCheckAgentPrograms_RelativeOverride_OutsideRepo_HonestMessageNotReinstall(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo; no binary anywhere
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.Len(t, findings, 2, "default_program and program_overrides each produce a finding")
	for _, f := range findings {
		require.True(t, f.Actionable,
			"outside a repo today's check is kept (actionable): %+v", f)
		require.Contains(t, f.Detail, "did not resolve from this directory")
		require.Contains(t, f.Detail, "not inside a git repository")
		require.Contains(t, f.Detail, "session's workspace")
		require.NotContains(t, f.Detail, "not installed or not on PATH",
			"the honest message must not send the user to reinstall")
	}
}

// TestCheckAgentBinaries_RelativeOverride_AnchoredToRepoRoot_NotCwd is the
// full-sweep (`af doctor`) core guarantee, the analogue of the in-repo
// setup-path test for checkAgentBinaries. The bug made a configured agent FAIL
// here (actionable, exit-code impact) when the doctor's cwd lacked the binary;
// the fix anchors the resolution to the repo workspace and finds it.
func TestCheckAgentBinaries_RelativeOverride_AnchoredToRepoRoot_NotCwd(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdir(t, subdir)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"the relative override resolves against the repo workspace, not the cwd")
	require.False(t, row.Problem)
	require.Contains(t, row.Detail, filepath.Join(repo, "bin", "claude"))
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
	require.NotContains(t, agentsHeaderValue(rep), "claude=missing")
}

// TestCheckAgentBinaries_RelativeOverride_OutsideRepo_HonestRemediationNotReinstall
// pins the bug-report's "FAIL for a configured agent in the environment sweep"
// scenario when the doctor runs outside any repo: the narrowing keeps today's
// check (a configured agent's missing command is still a FAIL), but the
// remediation is the honest message — it does not say install or reinstall.
func TestCheckAgentBinaries_RelativeOverride_OutsideRepo_HonestRemediationNotReinstall(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo; no binary anywhere
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status,
		"today's check is kept: a configured agent's missing command is still a FAIL")
	require.True(t, row.Problem, "today's check is kept (actionable)")
	require.Contains(t, row.Remediation, "did not resolve from this directory")
	require.Contains(t, row.Remediation, "not inside a git repository")
	require.Contains(t, row.Remediation, "session's workspace")
	require.NotContains(t, row.Remediation, "not installed or not on PATH",
		"the honest message must not send the user to reinstall")
	require.Contains(t, agentsHeaderValue(rep), "claude=missing")
}

// TestCheckAgentBinaries_BareNamePresent_OutsideRepo_StillPasses pins that a
// bare name (default_program = "claude") found on PATH still PASSes when the
// doctor runs outside any repo: a successful check is reported as today's PASS
// regardless of anchoring.
func TestCheckAgentBinaries_BareNamePresent_OutsideRepo_StillPasses(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status)
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
}

// TestDoctorSetup_RelativeOverrideInRepo_AnchoredToRepoRootPasses is the
// end-to-end `af doctor --setup` guarantee: a relative override resolving at
// the repo root passes the full setup sweep when the doctor is run from a
// subdir of the same repo — the case the bug falsely failed. Exercises the fix
// through Run, so the workspace computation and the agent-program check are
// observed in their real call order alongside the rest of the setup checks.
func TestDoctorSetup_RelativeOverrideInRepo_AnchoredToRepoRootPasses(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir() // for fake tmux, on PATH
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")

	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n\n[program_overrides]\nclaude = \"bin/claude\"\n"), 0o644))
	subdir := filepath.Join(repo, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdir(t, subdir)

	report, err := Run(Options{
		Setup:     true,
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
	})
	require.NoError(t, err)
	require.Zero(t, report.UnresolvedCount(), "findings: %+v", report.Findings)
	require.True(t, okContains(report, "default_program"),
		"the relative override must resolve anchored to the repo root")
	require.True(t, okContains(report, "program_overrides.claude"))
}

// TestCheckProgramCommand_AnchoredOutsideRepoFallsBackToCheckCommand pins the
// helper's contract: inside a repo it uses preflight.CheckCommandAt; genuinely
// outside any repo (config.ErrNotGitRepository) it falls back to today's
// preflight.CheckCommand against the doctor's own cwd and reports outsideRepo
// true so the caller emits the honest message; a failed probe for any other
// reason (git missing from PATH, an unreadable .git, …) is not "outside a repo"
// and keeps today's classified remediation.
func TestCheckProgramCommand_AnchoredOutsideRepoFallsBackToCheckCommand(t *testing.T) {
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+"/bin")

	// Inside a repo (repoErr == nil) -> anchored via CheckCommandAt, honest message not used.
	_, _, outsideRepo := checkProgramCommand("/some/repo", nil, "claude")
	require.False(t, outsideRepo, "inside a repo the command is anchored via CheckCommandAt")

	// Genuinely outside any repo -> today's CheckCommand, honest message used.
	_, _, outsideRepo = checkProgramCommand("", config.ErrNotGitRepository, "bin/claude")
	require.True(t, outsideRepo, "outside a repo the caller emits the honest message")

	// Repo probe failed for another reason -> today's CheckCommand, honest message NOT used.
	_, _, outsideRepo = checkProgramCommand("", config.ErrRepoProbeUnanswered, "bin/claude")
	require.False(t, outsideRepo, "a failed probe is not 'outside a repo'; keep today's classified remediation")
}

// TestCheckProgramCommand_OutsideRepo_CwdIndependentCommandKeepsClassifiedRemediation
// pins the refinement to the codex finding on doctor/program_check.go: outside
// a repo the honest no-anchor message is reserved for commands whose
// resolution genuinely depends on the working directory. A bare name (resolved
// through PATH) and an absolute path resolve the same way regardless of cwd, so
// a failure there is a genuine install/permission problem and keeps today's
// classified ProgramError remediation — the doctor must not send the user to
// rerun from inside a repo just because a path didn't resolve from the doctor's
// incidental cwd (#6487).
func TestCheckProgramCommand_OutsideRepo_CwdIndependentCommandKeepsClassifiedRemediation(t *testing.T) {
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+"/bin")

	// Bare name on PATH: cwd-independent -> outsideRepo false even outside a repo.
	_, _, outsideRepo := checkProgramCommand("", config.ErrNotGitRepository, "claude")
	require.False(t, outsideRepo, "a bare name resolves through PATH regardless of cwd; keep classified remediation")

	// Absolute path: cwd-independent -> outsideRepo false even outside a repo.
	_, _, outsideRepo = checkProgramCommand("", config.ErrNotGitRepository, "/missing/codex")
	require.False(t, outsideRepo, "an absolute path resolves regardless of cwd; keep classified remediation")

	// Relative path: cwd-dependent -> outsideRepo true outside a repo (honest message).
	_, _, outsideRepo = checkProgramCommand("", config.ErrNotGitRepository, "bin/claude")
	require.True(t, outsideRepo, "a relative path is cwd-dependent; emit the honest message")

	// A leading-./ relative path is also cwd-dependent.
	_, _, outsideRepo = checkProgramCommand("", config.ErrNotGitRepository, "./venv/bin/claude")
	require.True(t, outsideRepo, "a ./-prefixed relative path is cwd-dependent; emit the honest message")
}
