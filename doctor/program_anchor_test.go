package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The bug: `af doctor` resolved relative program_overrides against its own
// cwd (preflight.CheckCommand's os.Getwd fallback), not against the session's
// launch directory. A relative override like `program_overrides.claude =
// "bin/claude"` flipped between PASS and FAIL purely on where the doctor was
// invoked from — a false failure outside the repo and a false pass against a
// decoy cwd. The fix adopts the daemon-side #5779 machinery: when the doctor
// runs inside a repo, the relative override resolves against the repo's
// workspace (the directory a session launches in); when outside any repo, the
// doctor emits an advisory rather than resolving against its incidental cwd.

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

// TestCheckAgentPrograms_RelativeOverride_OutsideRepo_IsAdvisoryNotActionable
// pins the doctor's response when it is invoked outside any git repository: a
// relative override cannot be anchored. The bug's cwd-resolved resolution
// produced a false failure ("Claude Code is not installed...") because the
// doctor's cwd lacked the binary. The fix reports an advisory that does NOT
// drive UnresolvedCount and does NOT send the reader to reinstall the binary.
func TestCheckAgentPrograms_RelativeOverride_OutsideRepo_IsAdvisoryNotActionable(t *testing.T) {
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
		require.False(t, f.Actionable,
			"an unanchored relative override is advisory, not a failure: %+v", f)
		require.Contains(t, f.Detail, "relative command")
		require.Contains(t, f.Detail, "not inside a git repository")
		require.NotContains(t, f.Detail, "not installed or not on PATH",
			"the advisory must not say install — the binary was never checked against a real anchor")
	}
	require.Zero(t, rep.UnresolvedCount(),
		"a relative override outside any repo must not drive the exit code")
}

// TestCheckAgentPrograms_AbsoluteOverride_OutsideRepo_StillActionableWhenMissing
// guards against the easy overcorrection of making every override advisory
// when outside a repo: an absolute path that does not exist CAN be verified
// (it resolves to itself), so it must still produce an actionable finding
// when missing — exactly as today.
func TestCheckAgentPrograms_AbsoluteOverride_OutsideRepo_StillActionableWhenMissing(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "/missing/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings)
	for _, f := range findings {
		require.True(t, f.Actionable, "an absolute missing override is still a failure")
		require.Contains(t, f.Detail, "is not runnable")
		require.Contains(t, f.Detail, "not installed or not on PATH")
	}
	require.NotZero(t, rep.UnresolvedCount())
}

// TestCheckAgentPrograms_BareName_OutsideRepo_StillChecked pins that outside a
// repo, a bare name (which relies on PATH, anchor-independent for absolute
// PATH entries) is still verified normally: present → PASS, missing →
// actionable FAIL. The advisory is reserved for genuinely relative commands.
func TestCheckAgentPrograms_BareName_OutsideRepo_StillChecked(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo

	// Present bare name → PASS.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	present := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentPrograms(present, rep)
	require.Equal(t, StatusPass, findCheck(t, rep, "default_program").Status,
		"a present bare name still resolves when outside a repo")

	// Missing bare name override → actionable FAIL (not advisory).
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	missing := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "definitely-absent-agent"},
	}
	rep2 := &Report{}
	checkAgentPrograms(missing, rep2)
	findings := findByCheck(rep2, "agent-program")
	require.NotEmpty(t, findings)
	for _, f := range findings {
		require.True(t, f.Actionable, "a missing bare name remains actionable outside a repo")
	}
	require.NotZero(t, rep2.UnresolvedCount())
}

// TestCheckAgentBinaries_RelativeOverride_AnchoredToRepoRoot_NotCwd is the
// full-sweep (`af doctor`) core guarantee, the analogue of the in-repo
// setup-path test for checkAgentBinaries. The bug made a configured agent
// FAIL here (actionable, exit-code impact) when the doctor's cwd lacked the
// binary; the fix anchors the resolution to the repo workspace and finds it.
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
	require.NotContains(t, agentsHeaderValue(rep), "claude=unverified")
}

// TestCheckAgentBinaries_RelativeOverride_OutsideRepo_IsWarnNotProblem pins
// the bug-report's "FAIL for a configured agent in the environment sweep"
// scenario: when the doctor runs outside any repo and the configured default
// agent has a relative override, the old behavior was an actionable FAIL
// (exit-code impact). The fix makes it a non-actionable WARN: the doctor
// cannot verify a relative override from outside a repo, and saying so as a
// WARN that drives no exit code is the correct stance.
func TestCheckAgentBinaries_RelativeOverride_OutsideRepo_IsWarnNotProblem(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo; no binary anywhere
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusWarn, row.Status,
		"a relative override the doctor cannot anchor is advisory, not a FAIL")
	require.False(t, row.Problem,
		"a relative override outside any repo must not drive the exit code")
	require.Contains(t, row.Detail, "relative command")
	require.Contains(t, row.Detail, "not inside a git repository")
	require.NotContains(t, row.Detail, "not installed or not on PATH",
		"the advisory must not say install")
	require.Contains(t, agentsHeaderValue(rep), "claude=unverified")
	require.NotContains(t, agentsHeaderValue(rep), "claude=present")
	require.NotContains(t, agentsHeaderValue(rep), "claude=missing")
	require.Zero(t, rep.UnresolvedCount())
}

// TestCheckAgentBinaries_RelativeOverride_OutsideRepo_DoesNotFalsePassAgainstDecoyCwd
// pins the bug-report's false-pass scenario: a decoy `bin/claude` in the
// doctor's cwd must NOT make the doctor report PASS for a relative override
// the session's repo does not contain. The fix refuses to resolve against
// the doctor's cwd and emits an advisory instead.
func TestCheckAgentBinaries_RelativeOverride_OutsideRepo_DoesNotFalsePassAgainstDecoyCwd(t *testing.T) {
	// Outside any git repo, but the cwd happens to contain a decoy at
	// <decoy>/bin/claude. With the bug the doctor resolved "bin/claude"
	// against the cwd and falsely PASSed; the fix emits an advisory.
	decoy := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(decoy, "bin"), 0o755))
	writeExecutable(t, filepath.Join(decoy, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, decoy)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusWarn, row.Status, "the decoy in the cwd must not yield a false PASS")
	require.False(t, row.Problem)
	require.NotContains(t, row.Detail, filepath.Join(decoy, "bin", "claude"),
		"the decoy cwd's binary must not appear in the detail as a resolved path")
	require.Contains(t, agentsHeaderValue(rep), "claude=unverified")
	require.NotContains(t, agentsHeaderValue(rep), "claude=present")
	require.Zero(t, rep.UnresolvedCount())
}

// TestCheckAgentBinaries_AbsoluteOverride_OutsideRepo_StillFailsWhenMissing
// mirrors the setup-path guard for the full sweep: absolute paths do not
// depend on the launch directory, so the doctor outside any repo still
// verifies them and reports a configured agent's missing absolute binary as
// a FAIL (problem=true).
func TestCheckAgentBinaries_AbsoluteOverride_OutsideRepo_StillFailsWhenMissing(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "/missing/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status)
	require.True(t, row.Problem, "a configured agent with a missing absolute binary is actionable")
	require.NotContains(t, row.Detail, "relative command")
	require.Contains(t, agentsHeaderValue(rep), "claude=missing")
	require.NotContains(t, agentsHeaderValue(rep), "claude=unverified")
}

// TestCheckAgentBinaries_BareNamePresent_OutsideRepo_StillPasses pins that a
// bare name (default_program = "claude") found on PATH still PASSes when the
// doctor runs outside any repo. The advisory is reserved for relative-path
// commands; overgeneralizing it to bare names would make `af doctor` refuse
// to verify the most common `default_program` shape for every user who runs
// it from a non-repo directory.
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

// TestCheckAgentBinaries_BareNameMissing_OutsideRepo_StillFails pins the
// missing-bare-name branch outside a repo: a configured default agent that is
// absent from PATH remains an actionable FAIL, not an advisory — the bare
// name was checked against PATH and genuinely not found.
func TestCheckAgentBinaries_BareNameMissing_OutsideRepo_StillFails(t *testing.T) {
	chdir(t, t.TempDir()) // outside any git repo
	empty := t.TempDir()
	t.Setenv("PATH", empty) // no claude on PATH
	cfg := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, row.Status, "a missing bare name remains a FAIL outside a repo")
	require.True(t, row.Problem)
	require.Contains(t, row.Remediation, "not installed or not on PATH")
	require.NotContains(t, row.Detail, "relative command",
		"a bare name is not a relative command, so the advisory must not fire")
}

// TestDoctorSetup_RelativeOverrideInRepo_AnchoredToRepoRootPasses is the
// end-to-end `af doctor --setup` guarantee: a relative override resolving at
// the repo root passes the full setup sweep when the doctor is run from a
// subdir of the same repo — the case the bug falsely failed. Exercises the
// fix through Run, so the workspace computation and the agent-program check
// are observed in their real call order alongside the rest of the setup
// checks.
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
