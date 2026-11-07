package doctor

// #6792: a doctor PASS for an agent program must mean "a session created now
// would find that program". Sessions launch in a clean worktree created from
// the repo's BASE tree — origin's default branch when the repo has one
// (resolveOriginHead reads origin/HEAD, then origin/main, then origin/master,
// falling back to HEAD; session/git/worktree_ops.go), so any executable the
// launch needs that is not a runnable file in THAT tree resolves in this
// checkout but is absent — or present but not runnable — at launch. #6740
// checked the current checkout's index (`git ls-files`), which misses every
// shape where the index or working tree disagrees with that base tree.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// setupDoctorRepoWithOrigin returns a repo with an `origin` remote whose
// default branch carries only the initial commit: work committed afterwards
// is "not pushed", work only staged is "staged-only", and work on another
// branch is "feature-branch" — all absent from the tree a new session's
// worktree checks out while `git ls-files` reports them tracked.
func setupDoctorRepoWithOrigin(t *testing.T) string {
	t.Helper()
	repo := setupDoctorRepo(t)
	origin := filepath.Join(testguard.CanonicalTempDir(t), "origin.git")
	require.NoError(t, exec.Command("git", "init", "--bare", origin).Run())
	// Push the initial commit under the deterministic branch name the base-ref
	// probe reads after origin/HEAD: origin/main.
	require.NoError(t, exec.Command("git", "-C", repo, "checkout", "-B", "main").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "remote", "add", "origin", origin).Run())
	require.NoError(t, exec.Command("git", "-C", repo, "push", "-u", "origin", "main").Run())
	return repo
}

// ---------------------------------------------------------------------------
// Item 1 — the check must run against the tree a session branches from, not
// the current checkout's index.
// ---------------------------------------------------------------------------

// TestCheckAgentBinaries_UnpushedRelativeOverride_WarnsNotPasses is the
// committed-but-not-pushed case: `git ls-files` reports bin/claude tracked, so
// #6740's check keeps the PASS, but the file is absent from origin's default
// branch — the tree a new session's worktree materializes.
func TestCheckAgentBinaries_UnpushedRelativeOverride_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepoWithOrigin(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude") // committed locally, deliberately NOT pushed
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusWarn, row.Status,
		"a committed-but-unpushed override is absent from origin's default branch — WARN, not PASS")
	require.False(t, row.Problem, "an absent-from-base override is advisory, not actionable")
	require.Contains(t, row.Detail, "origin's default branch")
	require.Contains(t, row.Detail, "absent at launch")
	require.Contains(t, row.Remediation, "push")
	require.Contains(t, row.Remediation, "setup_commands")
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
	require.Zero(t, rep.UnresolvedCount(), "an absent-from-base override is advisory, not actionable")
}

// TestCheckAgentPrograms_UnpushedRelativeOverride_WarnsNotPasses is the
// setup-path twin: the same committed-but-unpushed file must WARN on
// `af doctor --setup` too.
func TestCheckAgentPrograms_UnpushedRelativeOverride_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepoWithOrigin(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude") // committed locally, deliberately NOT pushed
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount(), "an absent-from-base override is advisory, not actionable")
	for _, field := range []string{"default_program", "program_overrides.claude"} {
		row := findCheck(t, rep, field)
		require.Equal(t, StatusWarn, row.Status,
			"a committed-but-unpushed override must WARN, not PASS: %s", field)
		require.False(t, row.Problem)
		require.Contains(t, row.Detail, "origin's default branch")
		require.Contains(t, row.Detail, "absent at launch")
		require.Contains(t, row.Remediation, "push")
		require.Contains(t, row.Remediation, "setup_commands")
	}
}

// TestCheckAgentPrograms_StagedOnlyRelativeOverride_WarnsNotPasses is the
// staged-but-uncommitted shape: the file is in the index (so `git ls-files`
// reports it tracked) but in no committed tree — a session's clean checkout
// will not contain it.
func TestCheckAgentPrograms_StagedOnlyRelativeOverride_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepoWithOrigin(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	require.NoError(t, exec.Command("git", "-C", repo, "add", "--", "bin/claude").Run()) // staged, not committed
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"a staged-but-uncommitted override is absent from the committed tree — WARN, not PASS")
	require.False(t, row.Problem)
	require.Contains(t, row.Detail, "absent at launch")
}

// TestCheckAgentPrograms_FeatureBranchRelativeOverride_WarnsNotPasses pins
// that the base tree is origin's default branch and not HEAD: bin/claude is
// committed on a feature branch, so HEAD's own tree contains it — yet a
// session branches from origin/main and would not find it.
func TestCheckAgentPrograms_FeatureBranchRelativeOverride_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepoWithOrigin(t)
	require.NoError(t, exec.Command("git", "-C", repo, "checkout", "-b", "feature").Run())
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude") // committed on `feature` — absent from origin/main
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"an override committed only on a feature branch is absent from origin's default branch — WARN, not PASS")
	require.Contains(t, row.Detail, "origin's default branch")
}

// ---------------------------------------------------------------------------
// Item 2 — a relative WRAPPER around an installed agent must be qualified the
// same way. `tools/ionice -c 3 claude` carries two executable obligations:
// tools/ionice is what the shell launches first, and claude is the detected
// agent. Only the agent's resolved path was recorded before, so an untracked
// wrapper the clean worktree cannot launch still reported PASS.
// ---------------------------------------------------------------------------

func TestCheckAgentPrograms_UntrackedRelativeWrapper_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	writeExecutable(t, filepath.Join(repo, "tools"), "ionice", "#!/bin/sh\nexit 0\n")
	// tools/ionice is deliberately NOT committed.
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "tools/ionice -c 3 claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"an untracked relative wrapper must WARN even though the wrapped agent resolves on PATH")
	require.False(t, row.Problem)
	require.Contains(t, row.Detail, "tools/ionice")
	require.Contains(t, row.Detail, "absent at launch")
	require.Contains(t, row.Remediation, "commit the file")
	require.Contains(t, row.Remediation, "setup_commands")
}

// TestCheckAgentPrograms_CommittedRelativeWrapper_StillPasses is the no-false-
// positive guard for the same command: once the wrapper is in the base tree,
// both executable obligations hold in the worktree and the check stays PASS.
func TestCheckAgentPrograms_CommittedRelativeWrapper_StillPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	writeExecutable(t, filepath.Join(repo, "tools"), "ionice", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "tools/ionice")
	binDir := t.TempDir()
	writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "tools/ionice -c 3 claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	require.Zero(t, rep.UnresolvedCount())
	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"a committed wrapper plus a PATH-resolved agent satisfies the worktree — PASS")
}

// ---------------------------------------------------------------------------
// Item 3 — cwd dependence must come from the RESOLVED executables, not a
// first-token re-parse of the command. The old token split understood leading
// assignments and a bare `env`, so `env -i`, `env -u`, `env -C`, quoted
// targets, and relative PATH= entries all slipped the qualification even
// though their resolved target is relative to the launch directory.
// ---------------------------------------------------------------------------

// prefixedOverrideRepo builds a repo whose relative override targets exist in
// the working tree (each command resolves) but are not committed (each is
// absent from the tree a session checks out).
func prefixedOverrideRepo(t *testing.T) string {
	t.Helper()
	repo := setupDoctorRepo(t)
	for _, d := range []string{"bin", filepath.Join("sub", "bin"), "tools"} {
		require.NoError(t, os.MkdirAll(filepath.Join(repo, d), 0o755))
	}
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(repo, "bin"), "my claude", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(repo, "sub", "bin"), "claude", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(repo, "tools"), "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)
	return repo
}

// The commands share the fixtures: env chdir lands in sub/, env PATH= and the
// shell PATH= land in tools/, the plain relative targets land in bin/, and
// "bin/my claude" is the quoted-path-with-a-space shape.
var prefixedRelativeCommands = []string{
	"env -i bin/claude",          // env -i clears the environment; the target is still relative
	"env -u FOO bin/claude",      // env -u unsets one variable; the target is still relative
	`env FOO="a b" bin/claude`,   // env carrying a quoted assignment value
	"env -C sub bin/claude",      // env -C itself moves the launch directory
	"env -i PATH=tools claude",   // a relative PATH= entry inside env -i
	"PATH=tools claude",          // a relative PATH= as a leading shell assignment
	`env -u FOO "bin/my claude"`, // a quoted relative path containing a space
}

func TestCheckAgentPrograms_PrefixedRelativeOverride_WarnsNotPasses(t *testing.T) {
	prefixedOverrideRepo(t)
	for _, command := range prefixedRelativeCommands {
		cfg := &config.Config{
			DefaultProgram:   tmux.ProgramClaude,
			ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
		}
		rep := &Report{}
		checkAgentPrograms(cfg, rep)

		row := findCheck(t, rep, "program_overrides.claude")
		require.Equal(t, StatusWarn, row.Status,
			"%q: the resolved target is relative to the launch directory and absent from the base tree — WARN, not PASS",
			command)
		require.False(t, row.Problem)
		require.Contains(t, row.Detail, "absent at launch", "command %q", command)
	}
}

// TestCheckAgentPrograms_PrefixedRelativeOverride_StillPasses is the same
// command table with every target committed: a runnable file in the base tree
// keeps the PASS — the qualification must not downgrade the tracked case.
func TestCheckAgentPrograms_PrefixedRelativeOverride_StillPasses(t *testing.T) {
	repo := prefixedOverrideRepo(t)
	for _, rel := range []string{"bin/claude", "bin/my claude", "sub/bin/claude", "tools/claude"} {
		commitRepoFile(t, repo, rel)
	}
	for _, command := range prefixedRelativeCommands {
		cfg := &config.Config{
			DefaultProgram:   tmux.ProgramClaude,
			ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
		}
		rep := &Report{}
		checkAgentPrograms(cfg, rep)

		row := findCheck(t, rep, "program_overrides.claude")
		require.Equal(t, StatusPass, row.Status,
			"%q: a committed relative target satisfies the clean-checkout worktree — PASS", command)
	}
}

// ---------------------------------------------------------------------------
// Item 4 — a remote backend must be USABLE before doctor reports a skip.
// BackendConfigError covers missing config keys only; the missing backend
// CLI, a missing `origin` remote, a non-local Docker engine, and an
// unrunnable hook command are runtime facts BackendUnusableReason checks
// side-effect-free — the same call a default create fails on.
// ---------------------------------------------------------------------------

// TestCheckAgentBinaries_HookBackendCommandMissing_FailsNotSkips: a hook
// backend whose launch_cmd is not on PATH fails every create, so the sweep
// must FAIL rather than skipping host checks and exiting clean.
func TestCheckAgentBinaries_HookBackendCommandMissing_FailsNotSkips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "backend = \"hook\"\ndefault_program = \"claude\"\n[remote_hooks]\nlaunch_cmd = \"af-test-missing-hook-cmd\"\ndelete_cmd = \"/bin/true\"\n")
	chdir(t, repo)
	restore := session.SetLookPathForTest(func(name string) (string, error) {
		if name == "af-test-missing-hook-cmd" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	})
	defer restore()

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"a remote backend whose hook command cannot run must FAIL, not silently skip host checks")
	require.True(t, row.Problem)
	require.Contains(t, row.Detail, "not usable")
	require.Contains(t, row.Detail, "af-test-missing-hook-cmd")
	for _, c := range rep.Checks {
		require.NotEqual(t, tmux.ProgramClaude, c.Name,
			"no per-agent binary row should run when the remote backend is unusable")
	}
	require.NotZero(t, rep.UnresolvedCount(), "an unusable remote backend must drive the exit code")
}

// TestCheckAgentBinaries_SSHBackendNoOrigin_FailsNotSkips: an ssh backend in a
// repo with no `origin` remote fails every create (the runtime clones the
// workspace from origin), so the sweep must FAIL rather than skip.
func TestCheckAgentBinaries_SSHBackendNoOrigin_FailsNotSkips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t) // deliberately no `origin` remote
	writeInRepoConfig(t, repo, "backend = \"ssh\"\ndefault_program = \"claude\"\n[ssh]\nhost = \"agent-host.internal\"\n")
	chdir(t, repo)
	// The ssh CLI check is hermetic — the test asserts the missing origin, not
	// whether this host happens to ship `ssh`.
	restore := session.SetLookPathForTest(func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	defer restore()

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"a remote backend without an `origin` remote must FAIL, not silently skip host checks")
	require.True(t, row.Problem)
	require.Contains(t, row.Detail, "not usable")
	require.Contains(t, row.Detail, "origin")
	require.NotZero(t, rep.UnresolvedCount())
}

// TestCheckAgentPrograms_RemoteBackendUnusable_ActionableNotSkips is the
// setup-path twin: a hook backend whose command cannot run must produce an
// actionable finding, not a silent skip — and a skip it does report is an
// informational row, never a PASS for a check that never ran.
func TestCheckAgentPrograms_RemoteBackendUnusable_ActionableNotSkips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupDoctorRepo(t)
	writeInRepoConfig(t, repo, "backend = \"hook\"\ndefault_program = \"claude\"\n[remote_hooks]\nlaunch_cmd = \"af-test-missing-hook-cmd\"\ndelete_cmd = \"/bin/true\"\n")
	chdir(t, repo)
	restore := session.SetLookPathForTest(func(name string) (string, error) {
		if name == "af-test-missing-hook-cmd" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + name, nil
	})
	defer restore()

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	findings := findByCheck(rep, "agent-program")
	require.NotEmpty(t, findings,
		"an unusable remote backend must produce a finding, not a silent skip")
	require.Contains(t, findings[0].Detail, "not usable")
	require.Contains(t, findings[0].Detail, "af-test-missing-hook-cmd")
	require.True(t, rep.UnresolvedCount() > 0,
		"an unusable remote backend must drive the exit code")
}

// ---------------------------------------------------------------------------
// Item 5 — resolution failures must name the layer that actually failed. A
// malformed LEGACY repo config (~/.agent-factory/repos/<id>/config.json) fails
// the same resolve as a malformed in-repo file, but the doctor blamed "the
// in-repo config" either way and the underlying error named no file.
// ---------------------------------------------------------------------------

func TestCheckAgentBinaries_LegacyRepoConfigError_NamesTheFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repo := setupDoctorRepo(t)
	legacyDir := filepath.Join(home, "repos", config.RepoIDFromRoot(repo))
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	legacyFile := filepath.Join(legacyDir, config.RepoConfigFileName)
	require.NoError(t, os.WriteFile(legacyFile, []byte("{"), 0o644))
	chdir(t, repo)

	cfg := globalConfigWithoutOverrides()
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	row := findCheckByName(t, rep, "agent-config")
	require.Equal(t, StatusFail, row.Status,
		"an unresolvable legacy repo config is an actionable config failure")
	require.NotContains(t, row.Detail, "in-repo config",
		"the failing layer is the LEGACY repo config — the doctor must not blame the checked-in in-repo file")
	require.Contains(t, row.Detail, legacyFile,
		"the resolution error must name the file that failed to parse")
	require.NotZero(t, rep.UnresolvedCount())
}

// ---------------------------------------------------------------------------
// Review follow-ups (PR #6923) — three qualification edges in the same
// machinery: an absolute env -C breaks launch-dir derivation, a bare name's
// search continues past an absent match, and the base-tree probe must never
// promisor-fetch.
// ---------------------------------------------------------------------------

// An ABSOLUTE env -C replaces the modeled working dir with a fixed literal:
// `env -C <repo> bin/claude` resolves the same file no matter which
// directory the session starts in, so the target is not worktree content and
// the base-tree qualification must not downgrade the PASS — even though the
// resolved path lands inside this checkout.
func TestCheckAgentPrograms_EnvChdirAbsolute_KeepsPass(t *testing.T) {
	repo := prefixedOverrideRepo(t) // bin/claude exists but is untracked
	command := "env -C " + repo + " bin/claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"an absolute env -C pins the resolution to a fixed dir — the worktree contents are irrelevant")
	require.Contains(t, row.Detail, filepath.Join(repo, "bin", "claude"))
}

// A bare name's lookup does not stop at the absent match: with
// `env PATH=tools:<installed> claude`, the session's search skips the
// worktree-missing tools/claude and runs the installed fallback. The warning
// must name that rescue — claiming the program is simply absent at launch
// misleads about what the session actually runs.
func TestCheckAgentPrograms_BareNameFallback_NamesTheRescue(t *testing.T) {
	prefixedOverrideRepo(t) // tools/claude exists but is untracked
	fallback := testguard.CanonicalTempDir(t)
	writeExecutable(t, fallback, "claude", "#!/bin/sh\nexit 0\n")
	command := "env PATH=tools:" + fallback + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"the override's selected file is absent at launch — but the session still resolves the name")
	require.Contains(t, row.Detail, "falls back")
	require.Contains(t, row.Detail, filepath.Join(fallback, "claude"),
		"the warning must name the file the session actually runs")
	require.False(t, row.Problem)
}

// The base-tree probe answers from the ref's TREE object — never
// `git cat-file -e <ref>:<path>`, which dereferences the promised object and
// lazily fetches it in a blob:none partial clone. Probes run with
// GIT_NO_LAZY_FETCH so an unreadable object answers "unknown", not a fetch.
func TestBaseTreeProbe_UsesLsTreeWithoutFetching(t *testing.T) {
	var calls [][]string
	orig := gitTreeRead
	gitTreeRead = func(workspace string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		return nil, errProbeStub
	}
	defer func() { gitTreeRead = orig }()

	require.Equal(t, baseTreeUnknown, baseTreeProbe(t.TempDir(), "HEAD", "bin/claude"))
	require.Len(t, calls, 1)
	require.Equal(t, "ls-tree", calls[0][0],
		"presence+mode come from the tree object alone — cat-file -e would lazily fetch promised objects")
}

func TestGitTreeCmd_DisablesLazyFetchAndBoundsTheProbe(t *testing.T) {
	cmd := gitTreeCmd(context.Background(), t.TempDir(), "ls-tree", "-z", "HEAD")
	require.Equal(t, "git", filepath.Base(cmd.Path))
	require.Contains(t, cmd.Env, "GIT_NO_LAZY_FETCH=1",
		"object access must fail rather than promisor-fetch — a lazy fetch is network access and an object-store mutation")
}

var errProbeStub = errors.New("probe stub")

// ---------------------------------------------------------------------------
// Review follow-ups, round 2 (PR #6923) — three more edges in the same walk:
// PATH locations unrunnable only in THIS checkout still count as launch
// candidates, an unknown probe result never downgrades the PASS, and an
// opaque wrapper's unproven launch dir retains the qualification.
// ---------------------------------------------------------------------------

// A PATH location whose file is committed but deleted in this checkout is
// still where the session's lookup lands: `PATH=tools:basebin claude` with
// tools/claude untracked and basebin/claude committed-then-removed resolves
// tools/claude here but basebin/claude in the clean worktree. The candidate
// list must carry locations the checkout cannot run so the warning names the
// real launch answer.
func TestCheckAgentPrograms_BaseOnlyFallback_NamesTheRescue(t *testing.T) {
	repo := prefixedOverrideRepo(t) // tools/claude exists but is untracked
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "basebin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "basebin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "basebin/claude")
	require.NoError(t, os.Remove(filepath.Join(repo, "basebin", "claude"))) // deleted locally, still committed

	command := "PATH=tools:basebin claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"the checkout's selection is absent at launch — but the session resolves the committed file")
	require.Contains(t, row.Detail, "falls back")
	require.Contains(t, row.Detail, "basebin/claude",
		"the warning must name the committed file the session actually runs — even though this checkout cannot run it")
}

// The same file EARLIER in PATH order shadows the selection entirely:
// `PATH=basebin:tools claude` resolves tools/claude in this checkout because
// the committed basebin/claude is deleted locally — but the worktree
// contains it, so the session never reaches tools/claude at all.
func TestCheckAgentPrograms_BaseOnlyShadow_WarnsNotPasses(t *testing.T) {
	repo := prefixedOverrideRepo(t) // tools/claude exists but is untracked
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "basebin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "basebin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "basebin/claude")
	require.NoError(t, os.Remove(filepath.Join(repo, "basebin", "claude"))) // deleted locally, still committed

	command := "PATH=basebin:tools claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"the session resolves the committed file first — the checkout's selection never runs")
	require.Contains(t, row.Detail, "basebin/claude")
}

// An UNKNOWN probe result is inconclusive, not absence: when the tree read
// fails (a promised object that will not fetch, a corrupt tree) the check
// fails closed — it reports the launch resolution as unverified rather than
// asserting the file is absent, claiming a fallback will run, or letting the
// override pass unqualified.
func TestCheckAgentPrograms_UnknownProbe_ReportsUnverified(t *testing.T) {
	prefixedOverrideRepo(t) // tools/claude exists but is untracked
	fallback := testguard.CanonicalTempDir(t)
	writeExecutable(t, fallback, "claude", "#!/bin/sh\nexit 0\n")
	orig := gitTreeRead
	gitTreeRead = func(workspace string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return orig(workspace, args...) // the ref resolves; only the tree read fails
		}
		return nil, errProbeStub
	}
	defer func() { gitTreeRead = orig }()

	command := "env PATH=tools:" + fallback + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"an unproven probe must not pass silently — the check fails closed and says it cannot verify")
	require.Contains(t, row.Detail, "could not verify")
	require.Contains(t, row.Detail, filepath.Join(fallback, "claude"),
		"a known rescue is hedged as 'if it is absent', not asserted")
	require.False(t, row.Problem)
}

// An opaque wrapper leaves the modeled launch dir UNPROVEN, not independent:
// `tools/wrapper bin/claude` with a committed `exec "$@"` wrapper and an
// untracked target resolved in the workspace fails in the clean worktree —
// the qualification is retained, reported as unverified rather than asserted.
func TestCheckAgentPrograms_OpaqueWrapperTarget_KeepsQualification(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "tools"), "wrapper", "#!/bin/sh\nexec \"$@\"\n")
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "tools/wrapper") // the wrapper itself is in the tree; bin/claude is not
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "tools/wrapper bin/claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"an opaque wrapper's launch dir is unproven — the relative target's qualification must be retained")
	require.Contains(t, row.Detail, "cannot prove",
		"the resolution is unverified — the warning must say so rather than asserting it")
	require.Contains(t, row.Detail, "unverified",
		"an unproven launch dir means the session's answer is unknown — never 'absent'")
	require.NotContains(t, row.Detail, "absent at launch",
		"what af cannot resolve is unknown, not absent")
}

// ---------------------------------------------------------------------------
// Review follow-ups, round 3 (PR #6926) — the launch walk must judge a
// committed symlink by its TARGET in the base tree (a dangling link stays
// dangling in the worktree) and must not probe locations after the launch
// winner is already found.
// ---------------------------------------------------------------------------

// A committed DANGLING symlink earlier in PATH does not shadow the
// selection: `PATH=tools:<installed> claude` with `tools/claude -> missing`
// resolves /installed/claude here — and resolves it in the worktree too,
// because the materialized link is still dangling. Judging the entry by its
// mode alone (120000) misreports a shadow.
func TestCheckAgentPrograms_DanglingSymlinkDoesNotShadow(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	require.NoError(t, os.Symlink("missing", filepath.Join(repo, "tools", "claude")))
	commitRepoFile(t, repo, "tools/claude") // committed as mode 120000, dangling in both checkouts
	installed := testguard.CanonicalTempDir(t)
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)

	command := "env PATH=tools:" + installed + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"a dangling committed symlink stays dangling in the worktree — the session resolves the same installed file")
}

// The same shape with a VALID link does shadow: `tools/claude -> ../bin/claude`
// committed alongside a committed executable resolves in the worktree, so the
// session runs the linked file and the warning names it.
func TestCheckAgentPrograms_ValidSymlinkShadow_WarnsNotPasses(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink(filepath.Join("..", "bin", "claude"), filepath.Join(repo, "tools", "claude")))
	commitRepoFile(t, repo, "tools/claude")
	commitRepoFile(t, repo, "bin/claude")
	installed := testguard.CanonicalTempDir(t)
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)
	// Locally deleted: the checkout selects /installed, the worktree resolves the link.
	require.NoError(t, os.Remove(filepath.Join(repo, "tools", "claude")))

	command := "env PATH=tools:" + installed + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"a valid committed symlink resolves in the worktree — it shadows the checkout's selection")
	require.Contains(t, row.Detail, "tools/claude")
}

// A committed executable reached THROUGH a symlinked directory resolves in
// the worktree exactly as in this checkout: `claude -> bin/claude` where
// `bin` itself is `bin -> realbin` lands on realbin/claude — the probe must
// follow intermediate links the way the worktree's filesystem will, not
// report the literal path absent.
func TestCheckAgentPrograms_SymlinkedDirTarget_ResolvesThroughTree(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "realbin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "realbin"), "claude", "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Symlink("realbin", filepath.Join(repo, "bin")))
	require.NoError(t, os.Symlink(filepath.Join("bin", "claude"), filepath.Join(repo, "claude")))
	commitRepoFile(t, repo, "realbin/claude")
	commitRepoFile(t, repo, "bin")
	commitRepoFile(t, repo, "claude")
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "./claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"claude -> bin/claude -> realbin/claude resolves in the worktree exactly as here — PASS")
}

// An unproven location BEFORE the apparent winner voids the conclusion:
// `PATH=tools:<installed> claude` selects /installed in this checkout, but
// when tools/claude's tree read fails, the session may still resolve a
// committed tools/claude first — the lookup must report unverified, not an
// unqualified PASS.
func TestCheckAgentPrograms_UncertainEarlierLocation_ReportsUnverified(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	// No tools/claude locally — the checkout skips it; whether the worktree
	// holds it is exactly what the failed probe cannot answer.
	installed := testguard.CanonicalTempDir(t)
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	orig := gitTreeRead
	gitTreeRead = func(workspace string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return orig(workspace, args...) // the ref resolves; only tree reads fail
		}
		return nil, errProbeStub
	}
	defer func() { gitTreeRead = orig }()
	chdir(t, repo)

	command := "env PATH=tools:" + installed + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"an unproven earlier location means the launch winner is unestablished — fail closed, not PASS")
	require.Contains(t, row.Detail, "could not verify")
	require.False(t, row.Problem)
}

// Once the checkout-selected executable is proven the launch winner, no
// further location is probed: `PATH=<installed>:bin claude` probes nothing
// past /installed — trailing relative entries cannot change the answer and
// each could otherwise cost a full gitProbeTimeout.
func TestCheckAgentPrograms_NoProbesPastLaunchWinner(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "bin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "bin"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "bin/claude") // trailing location: committed, but never reached
	installed := testguard.CanonicalTempDir(t)
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)

	var lsTreeCalls int
	orig := gitTreeRead
	gitTreeRead = func(workspace string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ls-tree" {
			lsTreeCalls++
		}
		return orig(workspace, args...)
	}
	defer func() { gitTreeRead = orig }()

	command := "env PATH=" + installed + ":bin claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status)
	require.Zero(t, lsTreeCalls,
		"the checkout's selection is already the launch winner — probing the trailing relative entry is wasted")
}

// stubUsableDockerBackend makes BackendUnusableReason's docker prerequisites
// pass hermetically: a `docker` CLI on PATH, a local engine endpoint, and an
// `origin` remote — the same side-effect-free facts a create would need.
func stubUsableDockerBackend(t *testing.T, repo string) {
	t.Helper()
	// The host's DOCKER_HOST/DOCKER_CONTEXT win over `docker context inspect`,
	// so clear them — the endpoint then comes from the stubbed CLI answer.
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	restoreLookPath := session.SetLookPathForTest(func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	t.Cleanup(restoreLookPath)
	restoreExec := session.SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		return []byte("unix:///var/run/docker.sock\n"), nil
	})
	t.Cleanup(restoreExec)
	require.NoError(t, exec.Command("git", "-C", repo, "remote", "add", "origin",
		"https://example.invalid/repo.git").Run())
}

// ---------------------------------------------------------------------------
// Review follow-ups, round 4 (PR #6929 head) — ".." inside a committed link
// target is POSIX traversal af cannot prove inside the tree; trailing PATH
// entries after an absolute selection cannot gate launch dependence; and a
// proven earlier winner bounds the uncertainty scan.
// ---------------------------------------------------------------------------

// A committed link chain whose intermediate dir-link target contains ".."
// cannot be proven inside the tree: `claude -> bin/claude` with
// `bin -> missing/../realbin` resolves locally only because an untracked
// empty `missing/` exists — a clean checkout has no `missing`, so the link
// dangles. af must report the lookup unverified, never runnable-or-absent.
func TestCheckAgentPrograms_DotDotLinkTarget_ReportsUnverified(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "realbin"), 0o755))
	writeExecutable(t, filepath.Join(repo, "realbin"), "claude", "#!/bin/sh\nexit 0\n")
	// Untracked empty dir: makes `missing/../realbin` resolve locally but it
	// is absent from the committed tree, so the worktree's link dangles.
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "missing"), 0o755))
	require.NoError(t, os.Symlink("missing/../realbin", filepath.Join(repo, "bin"))) // raw: Join would clean the ".."
	require.NoError(t, os.Symlink(filepath.Join("bin", "claude"), filepath.Join(repo, "claude")))
	commitRepoFile(t, repo, "realbin/claude")
	commitRepoFile(t, repo, "bin")
	commitRepoFile(t, repo, "claude")
	chdir(t, repo)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: "./claude"},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status,
		"a '..' traversal af cannot prove inside the tree must be unverified — not runnable, not absent")
	require.Contains(t, row.Detail, "could not verify")
}

// Trailing relative PATH entries after an absolute selection are unreachable
// at launch: `env PATH=<installed>:bin tools/wrapper claude` under an opaque
// wrapper resolves the absolute installed binary in any launch dir, so the
// unproven launch dir must not turn the check unverified.
func TestCheckAgentPrograms_TrailingRelativePathEntry_DoesNotGate(t *testing.T) {
	repo := setupDoctorRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "tools"), 0o755))
	writeExecutable(t, filepath.Join(repo, "tools"), "wrapper", "#!/bin/sh\nexec \"$@\"\n")
	commitRepoFile(t, repo, "tools/wrapper")
	installed := testguard.CanonicalTempDir(t)
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	chdir(t, repo)

	command := "env PATH=" + installed + ":bin tools/wrapper claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"the absolute installed claude wins in every launch dir — the trailing relative entry is unreachable")
}

// A proven launch winner BEFORE the selection bounds the uncertainty scan:
// `PATH=basebin:missing:tools claude` resolves basebin/claude (committed,
// locally deleted) at launch before ever reaching the unprobed middle entry,
// so the verdict is the proven shadowing — not an unverifiable lookup.
func TestCheckAgentPrograms_EarlierWinnerBoundsUncertaintyScan(t *testing.T) {
	repo := setupDoctorRepo(t)
	for _, d := range []string{"basebin", "tools"} {
		require.NoError(t, os.MkdirAll(filepath.Join(repo, d), 0o755))
	}
	writeExecutable(t, filepath.Join(repo, "basebin"), "claude", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(repo, "tools"), "claude", "#!/bin/sh\nexit 0\n")
	commitRepoFile(t, repo, "basebin/claude")
	// Locally deleted: the checkout selects tools/claude; the worktree gets basebin/claude.
	require.NoError(t, os.Remove(filepath.Join(repo, "basebin", "claude")))
	chdir(t, repo)

	command := "env PATH=basebin:missing:tools claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusWarn, row.Status)
	require.Contains(t, row.Detail, "basebin/claude",
		"the proven earlier winner decides the lookup — the unprobed middle entry cannot unverify it")
	require.NotContains(t, row.Detail, "could not verify",
		"the winner is proven — the verdict must assert the shadowing, not plead uncertainty")
}

// A committed dir-link whose ABSOLUTE target contains ".." is resolved
// component-wise by the kernel — `tools -> <base>/missing/../installed`
// dangles while <base>/missing is absent, so PATH=tools:<installed> selects
// the installed binary in every checkout and no shadowing may be reported.
func TestCheckAgentPrograms_AbsDotDotLinkTarget_DoesNotShadow(t *testing.T) {
	repo := setupDoctorRepo(t)
	base := testguard.CanonicalTempDir(t)
	installed := filepath.Join(base, "installed")
	require.NoError(t, os.MkdirAll(installed, 0o755))
	writeExecutable(t, installed, "claude", "#!/bin/sh\nexit 0\n")
	// Raw concatenation keeps the ".." — filepath.Join would clean it away.
	target := filepath.Join(base, "missing") + "/../installed"
	require.NoError(t, os.Symlink(target, filepath.Join(repo, "tools")))
	commitRepoFile(t, repo, "tools")
	chdir(t, repo)

	command := "env PATH=tools:" + installed + " claude"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: command},
	}
	rep := &Report{}
	checkAgentPrograms(cfg, rep)

	row := findCheck(t, rep, "program_overrides.claude")
	require.Equal(t, StatusPass, row.Status,
		"the committed link dangles through '..' on the host too — installed/claude wins everywhere")
}
