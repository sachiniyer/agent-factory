package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/preflight"
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
// resolution to the repo's workspace, the same anchor the launch uses. The
// file is committed so the tracked-file qualification (#6499) keeps the PASS:
// a tracked relative override genuinely predicts the clean-checkout worktree.
func TestCheckAgentPrograms_RelativeOverride_AnchoredToRepoRoot_NotCwd(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude")
	// Run from a SUBDIR of the repo, itself reached through a symlink: the
	// binary lives at the repo root, not under the cwd, so cwd-relativity
	// would resolve to <subdir>/bin/claude (missing) while
	// workspace-relativity (the fix) resolves to <repo>/bin/claude
	// (present) — and the launch must not care that the repo's path goes
	// through a symlink (#6729).
	subdir := filepath.Join(repoViaSymlink(t, repo), "subdir")
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
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row),
		"the resolved path must be in the repo, not the cwd")
	override := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, override.Status)
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, override))
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
	commitRepoFile(t, repo, "bin/claude")
	// The repo is reached through a symlink for the same reason as the
	// setup-path test above: the anchor must hold when the repo's own path
	// goes through one (#6729).
	subdir := filepath.Join(repoViaSymlink(t, repo), "subdir")
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
	require.Equal(t, filepath.Join(repo, "bin", "claude"), reportedProgramPath(t, row))
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
	// SocketTempDir, not t.TempDir: the run resolves the daemon control
	// socket under this home, so <home>/daemon.sock must fit sun_path (103
	// bytes on darwin). A t.TempDir() home under macOS's ~48-byte
	// /var/folders/<hash>/T/ root, plus this test's long name, lands the
	// socket path past it and fails the unrelated `daemon` row — the #1940
	// guard reporting a home too deep to host a socket, which has nothing
	// to do with the anchor this test asserts (#6729).
	home := testguard.SocketTempDir(t)
	binDir := t.TempDir() // for fake tmux, on PATH
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude")
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")

	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n\n[program_overrides]\nclaude = \"bin/claude\"\n"), 0o644))
	subdir := filepath.Join(repoViaSymlink(t, repo), "subdir")
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

// repoViaSymlink returns a second path that reaches repo through a real
// symlink, so the anchored-resolution tests exercise a symlinked repo path
// on every platform instead of only where the temp root happens to sit
// behind one (macOS's /var -> /private/var, a symlinked TMPDIR). Making the
// hop explicit is what keeps a canonicalized fixture from hiding the case
// later (#6729).
func repoViaSymlink(t *testing.T, repo string) string {
	t.Helper()
	link := filepath.Join(filepath.Dir(repo), "repo-via-link")
	require.NoError(t, os.Symlink(repo, link))
	return link
}

// commitRepoFile stages and commits path (relative to repo) so it is part of
// the tracked tree — the tree a clean-checkout worktree materializes. The
// tracked-file qualification (#6499) downgrades an untracked relative override
// to a WARN, so the anchored-to-repo-root PASS tests must commit their
// override to stay PASS.
func commitRepoFile(t *testing.T, repo, path string) {
	t.Helper()
	require.NoError(t, exec.Command("git", "-C", repo, "add", "--", path).Run())
	require.NoError(t, exec.Command("git", "-C", repo, "commit", "-m", "track "+path).Run())
}

// reportedProgramPath returns the executable path a passing program check
// row's Detail ends with, canonicalized. The doctor reports the resolution
// in whatever spelling the platform's getcwd and git produced — the
// physical toplevel on Linux, the as-entered (possibly symlinked) spelling
// on darwin — so a path assertion has to compare canonical paths rather
// than the spelling the fixture used to reach the repo (#6729).
func reportedProgramPath(t *testing.T, row CheckResult) string {
	t.Helper()
	// The path is the last %s of the detail's "%q uses %s" /
	// "%q resolves to %s" format, so it is split out on the known delimiter —
	// never on whitespace, which a TMPDIR containing a space would cut inside
	// the path itself. The delimiter includes the %q command's closing quote
	// so a delimiter-looking substring inside the path cannot false-match.
	i, sep := strings.LastIndex(row.Detail, `" resolves to `), len(`" resolves to `)
	if j := strings.LastIndex(row.Detail, `" uses `); j > i {
		i, sep = j, len(`" uses `)
	}
	require.GreaterOrEqual(t, i, 0, "check row %q carries no resolved path: %q", row.Name, row.Detail)
	path := row.Detail[i+sep:]
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err, "check row %q reports a path that does not resolve: %q", row.Name, path)
	return resolved
}

// ---------------------------------------------------------------------------
// #6499: relative program_overrides that resolve in the main checkout but are
// untracked are absent from the clean-checkout worktree a session launches in.
// The doctor's repo-root anchor genuinely predicts the worktree only for
// tracked relative paths, so an untracked relative override must downgrade
// from PASS to an advisory WARN rather than reporting a guaranteed-false PASS.
// ---------------------------------------------------------------------------

// TestCheckAgentPrograms_RelativeOverrideUntracked_WarnsNotPasses is the
// setup-path core of the #6499 fix: a relative override that exists only as an
// untracked file in the main checkout resolves against the repo root (so the
// old code PASSed) but will be absent from the clean-checkout worktree a
// session launches in. The fix downgrades that PASS to an advisory WARN whose
// detail names the cause and whose remediation names the two supported fixes
// (commit the file, or provision it via setup_commands / the setup overlay).
func TestCheckAgentPrograms_RelativeOverrideUntracked_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	// Deliberately NOT committed: bin/claude is untracked, so it is absent
	// from a clean-checkout worktree.
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	// Advisory: the file does resolve in this checkout, so it must not drive
	// the exit code or the unresolved count.
	require.Zero(t, rep.UnresolvedCount(), "an untracked override is advisory, not actionable")
	for _, field := range []string{"default_program", "program_overrides.claude"} {
		row := findCheck(t, rep, field)
		require.Equal(t, StatusWarn, row.Status,
			"an untracked relative override must WARN, not PASS: %s", field)
		require.False(t, row.Problem, "an untracked override is advisory: %s", field)
		require.Contains(t, row.Detail, "not in HEAD")
		require.Contains(t, row.Detail, "clean checkouts")
		require.Contains(t, row.Detail, "absent at launch")
		require.Contains(t, row.Remediation, "commit the file")
		require.Contains(t, row.Remediation, "setup_commands")
	}
}

// TestCheckAgentBinaries_RelativeOverrideUntracked_WarnsNotPasses is the
// full-sweep analogue: checkAgentBinaries must also downgrade an untracked
// relative override from PASS to an advisory WARN, and the agents header must
// still report the agent as present (the file does resolve in this checkout).
func TestCheckAgentBinaries_RelativeOverrideUntracked_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusWarn, row.Status,
		"an untracked relative override must WARN, not PASS")
	require.False(t, row.Problem, "an untracked override is advisory")
	require.Contains(t, row.Detail, "not in HEAD")
	require.Contains(t, row.Detail, "clean checkouts")
	require.Contains(t, row.Detail, "absent at launch")
	require.Contains(t, row.Remediation, "commit the file")
	require.Contains(t, row.Remediation, "setup_commands")
	// The file does resolve in this checkout, so the header still reports
	// present — the WARN qualifies what the header claims, it does not
	// contradict it.
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
	require.Zero(t, rep.UnresolvedCount(), "an untracked override is advisory, not actionable")
}

// TestCheckAgentPrograms_TrackedRelativeOverride_StillPasses pins that the
// #6499 qualification does not regress the tracked case: a relative override
// pointing at a committed file still PASSes, because the repo-root anchor
// genuinely predicts the clean-checkout worktree for tracked paths.
func TestCheckAgentPrograms_TrackedRelativeOverride_StillPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude")
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount())
	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"a tracked relative override genuinely predicts the worktree and stays PASS")
}

// TestCheckAgentBinaries_AbsoluteOverrideUntracked_NotAffected pins that the
// #6499 qualification is scoped to relative paths: an absolute path resolves
// the same way in any worktree, so an untracked absolute override is not
// downgraded. (A real absolute path is used so the file genuinely resolves.)
func TestCheckAgentBinaries_AbsoluteOverrideUntracked_NotAffected(t *testing.T) {
	repo := setupDoctorRepo(t)
	bin := filepath.Join(repo, "bin", "claude")
	require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
	writeExecutable(t, filepath.Dir(bin), "claude", "#!/bin/sh\nexit 0\n")
	// Deliberately NOT committed, but the command is absolute so the
	// tracked-file qualification does not apply.
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: bin},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"an absolute override is not cwd-dependent; the untracked qualification does not apply")
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
}

// TestCheckAgentBinaries_BareNameOnPath_NotAffectedByUntrackedCheck pins that
// a bare name (resolved through PATH) is not cwd-dependent and so is not
// downgraded by the tracked-file qualification.
func TestCheckAgentBinaries_BareNameOnPath_NotAffectedByUntrackedCheck(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	chdir(t, setupDoctorRepo(t)) // inside a repo, but the command is a bare name

	cfg := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, row.Status,
		"a bare name resolves through PATH; the untracked qualification does not apply")
}

// TestDoctorSetup_RelativeOverrideUntracked_WarnsNotActionable is the
// end-to-end `af doctor --setup` guarantee: a relative override that exists
// only as an untracked file produces an advisory WARN (not a PASS, not a FAIL)
// and does not drive the exit code. Exercises the fix through Run, so the
// workspace computation and the agent-program check are observed in their real
// call order alongside the rest of the setup checks.
func TestDoctorSetup_RelativeOverrideUntracked_WarnsNotActionable(t *testing.T) {
	home := testguard.SocketTempDir(t)
	binDir := t.TempDir()
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	// Deliberately NOT committed: the override is untracked.
	writeExecutable(t, binDir, "tmux", "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'tmux 3.4'; exit 0; fi\nexit 0\n")

	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n\n[program_overrides]\nclaude = \"bin/claude\"\n"), 0o644))
	chdir(t, repo)

	report, err := Run(Options{
		Setup:     true,
		ConfigDir: home,
		TempDir:   t.TempDir(),
		remoteConfig: func() (*config.RemoteHooks, string, error) {
			return nil, "", nil
		},
	})
	require.NoError(t, err)
	// Advisory: the untracked override must not drive the exit code.
	require.Zero(t, report.UnresolvedCount(),
		"an untracked override is advisory, not actionable: %+v", report.Findings)
	// The WARN must be visible — not a PASS, and not silently absent.
	row := findCheck(t, report, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status)
	require.False(t, row.Problem)
	require.Contains(t, row.Detail, "not in HEAD")
	require.Contains(t, row.Detail, "clean checkouts")
	require.Contains(t, row.Remediation, "commit the file")
	require.Contains(t, row.Remediation, "setup_commands")
}

// TestOverrideAbsentFromSessionBase_HelperContract pins the helper's contract
// directly: only a working-dir-dependent resolution landing inside the
// workspace whose repo-relative path is absent — or present but not runnable
// — in the tree sessions branch from produces a warning. Committed runnable
// files, outside-workspace resolutions, cwd-independent resolutions, and
// probe failures all keep the PASS (#6792 items 1–3).
func TestOverrideAbsentFromSessionBase_HelperContract(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude")
	// Sibling that exists only in the working tree — absent from HEAD's tree
	// (this repo has no origin remote, so sessions branch from HEAD).
	writeExecutable(t, filepath.Join(repo, "bin"), "untracked", "#!/bin/sh\nexit 0\n")
	// Sibling committed WITHOUT the executable bit — in the tree, but the
	// worktree materializes it non-executable.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "bin", "noexec"), []byte("#!/bin/sh\n"), 0o644))
	commitRepoFile(t, repo, "bin/noexec")
	// Sibling staged but never committed — in the index, not in the tree.
	writeExecutable(t, filepath.Join(repo, "bin"), "staged", "#!/bin/sh\nexit 0\n")
	require.NoError(t, exec.Command("git", "-C", repo, "add", "--", "bin/staged").Run())

	tracked := filepath.Join(repo, "bin", "claude")
	untracked := filepath.Join(repo, "bin", "untracked")
	noexec := filepath.Join(repo, "bin", "noexec")
	staged := filepath.Join(repo, "bin", "staged")

	check := func(executable, path string, via bool) *preflight.ProgramCheck {
		return &preflight.ProgramCheck{
			Command: executable, Executable: executable, Path: path, PathViaWorkingDir: via,
		}
	}

	// Committed runnable file -> "" (PASS is kept).
	detail, _ := overrideAbsentFromSessionBase(repo, check("bin/claude", tracked, true))
	require.Empty(t, detail, "a committed relative override satisfies the base tree")

	// Untracked relative override -> absent detail.
	detail, _ = overrideAbsentFromSessionBase(repo, check("bin/untracked", untracked, true))
	require.Contains(t, detail, "not in HEAD")
	require.Contains(t, detail, "absent at launch")

	// Staged-only -> absent detail (in the index, but not in the tree).
	detail, _ = overrideAbsentFromSessionBase(repo, check("bin/staged", staged, true))
	require.Contains(t, detail, "absent at launch",
		"a staged-but-uncommitted file is absent from the committed tree")

	// Committed without the executable bit -> not-executable detail.
	detail, remediation := overrideAbsentFromSessionBase(repo, check("bin/noexec", noexec, true))
	require.Contains(t, detail, "executable")
	require.Contains(t, remediation, "chmod +x")

	// via=false -> "": an absolute token or an absolute-PATH resolution finds
	// the same file from any directory even when it lands inside the repo.
	detail, _ = overrideAbsentFromSessionBase(repo, check("bin/untracked", untracked, false))
	require.Empty(t, detail, "a cwd-independent resolution is not the worktree's problem")

	// Path outside the workspace -> "" (the worktree check does not apply).
	parent := filepath.Dir(repo)
	detail, _ = overrideAbsentFromSessionBase(repo,
		check(filepath.Join("..", "bin", "x"), filepath.Join(parent, "bin", "x"), true))
	require.Empty(t, detail, "a path resolving outside the workspace is not checked")

	// Empty workspace -> "" (no repo to check against).
	detail, _ = overrideAbsentFromSessionBase("", check("bin/untracked", untracked, true))
	require.Empty(t, detail, "an empty workspace means no repo to check against")

	// Git probe failure (non-existent workspace) -> "" (conservative).
	detail, _ = overrideAbsentFromSessionBase(filepath.Join(t.TempDir(), "no-such-repo"),
		check("bin/untracked", untracked, true))
	require.Empty(t, detail, "a git probe failure is conservative, not a downgrade")

	// Nil check -> "".
	detail, _ = overrideAbsentFromSessionBase(repo, nil)
	require.Empty(t, detail)

	// The wrapper candidate: the agent itself resolves on PATH, but the
	// relative wrapper is absent from the base tree -> wrapper detail (#6792
	// item 2).
	wrapped := &preflight.ProgramCheck{
		Command: "tools/ionice -c 3 claude", Executable: "claude", Path: "/usr/bin/claude",
		ShellExecutable: "tools/ionice", ShellPath: untracked, ShellViaWorkingDir: true,
	}
	detail, _ = overrideAbsentFromSessionBase(repo, wrapped)
	require.Contains(t, detail, "tools/ionice")
	require.Contains(t, detail, "absent at launch")
}

// TestCommandDependsOnCwd pins that the cwd-dependence gate reads the
// resolved executables — and their resolution's working-dir dependence —
// rather than the command's first token: `env -i bin/claude` counts while a
// bare name does not, and a bare name under a relative PATH= entry counts too
// (#6792 item 3). With no resolved check, the coarse commandToken split is
// the fallback and keeps its old answers.
func TestCommandDependsOnCwd(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"bin/claude", true},
		{"./venv/bin/claude", true},
		{"FOO=x bin/claude", true},
		{"FOO=x BAR=y bin/claude", true},
		{"env bin/claude", true},
		{"env FOO=x bin/claude", true},
		{"claude", false},
		{"env claude", false},
		{"/usr/bin/codex", false},
		{"FOO=x /usr/bin/codex", false},
		{"", false},
	} {
		require.Equal(t, tc.want, commandDependsOnCwd(nil, tc.command),
			"commandDependsOnCwd(nil, %q) = %v, want %v", tc.command, !tc.want, tc.want)
	}
	for _, tc := range []struct {
		name  string
		check *preflight.ProgramCheck
		want  bool
	}{
		{"relative executable resolved via the launch dir",
			&preflight.ProgramCheck{Executable: "bin/claude", Path: "/repo/bin/claude", PathViaWorkingDir: true}, true},
		{"relative executable under an absolute env -C resolves cwd-independently",
			&preflight.ProgramCheck{Executable: "bin/claude", Path: "/abs/bin/claude"}, false},
		{"bare executable", &preflight.ProgramCheck{Executable: "claude", Path: "/usr/bin/claude"}, false},
		{"bare executable resolved via a relative PATH entry",
			&preflight.ProgramCheck{Executable: "claude", Path: "/repo/tools/claude", PathViaWorkingDir: true}, true},
		{"absolute executable", &preflight.ProgramCheck{Executable: "/usr/bin/claude", Path: "/usr/bin/claude"}, false},
		{"relative wrapper failed before the agent resolved — tokens are all we know",
			&preflight.ProgramCheck{Executable: "claude", ShellExecutable: "tools/ionice"}, true},
		{"absolute wrapper failed before the agent resolved — the failure is not cwd-dependent",
			&preflight.ProgramCheck{Executable: "claude", ShellExecutable: "/opt/ionice"}, false},
		{"bare executable with a cwd-dependent wrapper",
			&preflight.ProgramCheck{Executable: "claude", Path: "/usr/bin/claude", ShellExecutable: "ionice",
				ShellPath: "/repo/tools/ionice", ShellViaWorkingDir: true}, true},
	} {
		require.Equal(t, tc.want, commandDependsOnCwd(tc.check, "claude"),
			"commandDependsOnCwd(%s) = %v, want %v", tc.name, !tc.want, tc.want)
	}
}
