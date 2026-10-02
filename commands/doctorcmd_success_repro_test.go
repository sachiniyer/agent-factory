package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/doctor"
	"github.com/sachiniyer/agent-factory/internal/testguard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the WHOLE af binary as a subprocess because the bug
// lives on the success (return nil) path of doctorCmd.RunE: the deferred
// log close runs after RunE returns, and log.Close writes its "wrote logs to
// <path>" hint directly to os.Stderr (log/log.go:565, fmt.Fprintln(os.Stderr,
// ...)), bypassing cobra's err writer that the in-process tests in
// doctorcmd_test.go capture via cmd.SetErr. So no in-process test in that
// shape can observe the leak channel even with a dirty canned report — the
// hint's target stream is outside its view. They reuse the same package-level
// afTestBinary seam (built once per test binary by sshrelaycmd_test.go) that
// doctorcmd_exit_test.go uses to ask "what does the whole program write to
// fd 2 on an exit-0 run".

// doctorDirtySuccessConfig dirties a doctor run while keeping doctorExitCode
// at 0:
//   - default_program = "claude" is a tmux.SupportedPrograms value, so config
//     validation accepts it (config rejects an unsupported default_program
//     with an enum error; an unsupported value would turn checkConfig into an
//     actionable FAIL and exit 1).
//   - program_overrides.claude = "true" makes checkAgentPrograms resolve
//     claude to the ever-present /bin/true (preflight.CheckCommand -> LookPath),
//     reporting PASS instead of an actionable missing-binary FAIL. The setup
//     profile only checks default_program and the listed overrides (setup.go
//     checkAgentPrograms), so the other unconfigured agents are never probed.
//   - unknown_key_in_config triggers config/config_parse.go warnUnknownTomlKeys
//     -> log.WarningLog (log/log.go dirtyWriter -> dirty.Store(true)), but the
//     key is warned-and-ignored, so config loads successfully and
//     reportConfigValidity reports a PASS. dirty is set, no finding is added:
//     exactly the dirty + exit 0 state the success-path deferred close must
//     handle mode-aware.
const doctorDirtySuccessConfig = `default_program = "claude"
program_overrides.claude = "true"
unknown_key_in_config = "will-warn"
`

// doctorSetupGitIdentity seeds $HOME/.gitconfig so checkGitIdentity passes
// without depending on the developer's real global git identity (the
// subprocess runs with HOME=home, so git reads home/.gitconfig as the global
// config when the agent-factory repo has no repo-level user.name/user.email).
const doctorSetupGitIdentity = `[user]
	name = af-doctor-test
	email = af-doctor-test@example.com
`

// requireTmuxOrSkip skips the test when tmux is absent. A --setup exit-0 run
// reports a missing tmux as an actionable FAIL (doctor/setup.go checkTmux ->
// addActionableFinding), which would make the exit-0 assertion fail for an
// environment reason rather than a regression. tmux is preinstalled on the
// ubuntu CI runners and present on any machine running af (af requires it);
// where it is absent this contract cannot be exercised, so the test skips
// rather than false-fails.
func requireTmuxOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: a --setup exit-0 doctor run reports a missing tmux as actionable; skipping: %v", err)
	}
}

// filterInheritedEnv strips Git's and the shell's startup environment from the
// inherited process environment so the child git and the spawned af read the
// fixture's $HOME/.gitconfig as the global config (and /etc/gitconfig as the
// system config), operate against the fixture's repoDir (cmd.Dir), and are not
// redirected at an external repository or identity the test runner pointed at,
// and so the child shell's startup resolves from the fixture HOME rather than
// the runner's.
//
// Three shapes of entry are dropped, mirroring the override classification in
// session/git/repository_environment.go (repositoryPathEnvironment), which
// already treats these names as Git environment that must not cross the
// boundary:
//
//   - Git config overrides: GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM (a file
//     path — both an empty and a non-empty value override $HOME/.gitconfig, so
//     the entry is dropped rather than blanked) and GIT_CONFIG_COUNT with its
//     indexed GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n pairs (plus GIT_CONFIG and
//     GIT_CONFIG_PARAMETERS — command-line config entries git applies on top of
//     the file config). If a runner exports these with empty user.name /
//     user.email values, checkGitIdentity sees the inherited empty identity
//     instead of the fixture's .gitconfig, so the asserted exit-0 run fails for
//     the runner's configuration rather than the regression under test. The
//     COUNT entry and every indexed KEY/VALUE entry are removed wholesale for
//     the same reason as the file overrides: an empty value still applies, so
//     they cannot be blanked. GIT_TEMPLATE_DIR is dropped for the same reason:
//     it points `git init` at a template directory whose .git/config can carry
//     an empty user.name/user.email that overrides the seeded $HOME/.gitconfig
//     and makes checkGitIdentity actionable for a runner-config reason rather
//     than the regression under test. GIT_CONFIG_NOSYSTEM (set in childEnv
//     below) already disables /etc/gitconfig, but GIT_TEMPLATE_DIR is a
//     separate env var that bypasses the file config, so it must be stripped
//     here; an empty value still points git at the default template, so the
//     entry is removed rather than blanked.
//   - Git repository-local variables: GIT_DIR, GIT_WORK_TREE,
//     GIT_IMPLICIT_WORK_TREE, GIT_OBJECT_DIRECTORY, GIT_ALTERNATE_OBJECT_DIRECTORIES,
//     GIT_INDEX_FILE, GIT_GRAFT_FILE, GIT_REPLACE_REF_BASE, GIT_PREFIX,
//     GIT_INTERNAL_SUPER_PREFIX, GIT_SHALLOW_FILE, GIT_COMMON_DIR — the names
//     `git rev-parse --local-env-vars` reports. When a test runs from a Git hook
//     or other caller exporting GIT_DIR or GIT_WORK_TREE, the `git init` in
//     runDoctorSuccessSubprocess would inherit them and initialize or reuse that
//     external repository rather than repoDir, and the spawned af would inherit
//     them and let checkGit (`git rev-parse --show-toplevel`) probe the caller's
//     repository — making this regression test non-hermetic. An empty GIT_DIR
//     still points git at the current directory, so these are removed, not
//     blanked.
//   - Git trace variables: the full GIT_TRACE* family — GIT_TRACE (general
//     tracing) and the named sub-traces GIT_TRACE_SETUP, GIT_TRACE_PERFORMANCE,
//     GIT_TRACE_PACKET, ... plus the GIT_TRACE2* family (GIT_TRACE2_PERF,
//     GIT_TRACE2_EVENT, GIT_TRACE2_BRIEF, ...). --setup invokes `git rev-parse`
//     and `git config` in checkGit/checkGitIdentity; with any tracing enabled Git
//     writes trace diagnostics to stderr (e.g. "trace: built-in: git rev-parse
//     --show-toplevel"), which breaks the empty-stderr assertion this test pins
//     even though the JSON log-close behavior is correct. Matching only the
//     GIT_TRACE2 prefix would leave the legacy GIT_TRACE_SETUP /
//     GIT_TRACE_PERFORMANCE variables in the child environment, so those would
//     still emit to stderr on a runner that exports them. These are not
//     repository-local (git rev-parse --local-env-vars does not list them), but
//     they are inherited the same way and produce stderr output the test must not
//     see, so they are stripped here too. An empty value still turns tracing on
//     for the boolean trace variables (an empty value means "1" per git's docs),
//     so the entry is dropped rather than blanked.
//   - Shell startup overrides: ZDOTDIR (zsh), BASH_ENV (non-interactive bash),
//     ENV (interactive POSIX sh), and HISTFILE — the names
//     internal/testguard/userhome.go clears for subprocesses (userRootOverrides),
//     which send a shell to startup files outside HOME. The spawned af's config
//     load runs the Claude shell probe (config/claude_probe.go GetClaudeCommand),
//     which spawns SHELL and sources its rc; an inherited ZDOTDIR would point zsh
//     at the runner's startup files instead of the fixture's $HOME, and an
//     inherited BASH_ENV / ENV sources an arbitrary runner file even before the
//     rc, causing startup side effects or the probe's five-second timeout. With
//     SHELL pinned to /bin/sh (set in childEnv below) the probe's else-branch runs
//     `sh -c "which claude"` and sources nothing, but other code paths in the
//     spawned af may still spawn a shell, so the overrides are dropped here too.
//     For these an empty value already means "no file" (bash/sh source BASH_ENV/
//     ENV only when non-empty, and an unset ZDOTDIR falls back to HOME), so
//     dropping them is what clears the redirect; it mirrors testguard's Unsetenv.
func filterInheritedEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL",
			"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_SYSTEM",
			"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_INDEX_FILE", "GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE",
			"GIT_PREFIX", "GIT_INTERNAL_SUPER_PREFIX", "GIT_SHALLOW_FILE",
			"GIT_COMMON_DIR",
			"GIT_TEMPLATE_DIR",
			"ZDOTDIR", "BASH_ENV", "ENV", "HISTFILE":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(name, "GIT_CONFIG_VALUE_") ||
			strings.HasPrefix(name, "GIT_TRACE") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runDoctorSuccessSubprocess runs the built af binary with `af doctor
// --setup --json` in a throwaway home the caller has already seeded, under a
// real PATH so the setup prerequisites (git for checkGit, tmux for checkTmux,
// /bin/true for the program_overrides.claude override) resolve to PASS. The
// af binary runs by absolute path from afTestBinary, so it does not need PATH
// to locate itself. Returns stdout, stderr, and the process exit code. This is
// the exit-0 counterpart of runDoctorSubprocess in doctorcmd_exit_test.go,
// which empties PATH to FORCE exit 1; this one needs exit 0, so PATH must
// resolve the setup prerequisites.
//
// cmd.Dir is a freshly-initialized throwaway git repo rather than the test
// binary's CWD: checkGit (doctor/setup.go) runs `git rev-parse --show-toplevel`
// against the subprocess's working directory, and the binary's CWD is the
// commands/ package — which is a git checkout in a normal checkout and on
// CI, but NOT when the module is built from a source archive or the module
// cache (no .git). Running from a temp git repo keeps the exit-0 assertion
// independent of whether the source tree happens to be a checkout, so the
// behavior under test (the success-path log close) is what the test pins.
func runDoctorSuccessSubprocess(t *testing.T, home string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	bin := afTestBinary(t)
	// checkGit needs a git repo for its CWD (doctor/setup.go:141); a temp
	// `git init` makes one without depending on the source tree being a
	// checkout, so the test is hermetic to where `go test` runs from.
	repoDir := t.TempDir()
	// The `git init` and the spawned af share one filtered, fixture-scoped
	// environment (childEnv). filterInheritedEnv strips the Git
	// config/repository/trace variables a runner may export so `git init`
	// targets repoDir (not an external GIT_DIR) and the child reads the
	// fixture's $HOME/.gitconfig as the global config. HOME and XDG_CONFIG_HOME
	// are pointed at the fixture home for BOTH commands: the --setup daemon
	// check resolves the systemd user dir from $XDG_CONFIG_HOME
	// (daemon/legacy_units.go defaultSystemdUserDir), and the `git init` reads
	// $HOME/.gitconfig for its own settings — in particular init.templateDir.
	// If `git init` kept the runner's HOME, a runner whose global git config
	// sets init.templateDir would copy that template's .git/config (which can
	// carry an empty user.name/user.email) into repoDir, where it takes
	// precedence over the fixture's later $HOME/.gitconfig and makes
	// checkGitIdentity actionable for a runner-config reason rather than the
	// regression under test. Giving initCmd the same fixture HOME means the
	// init reads the fixture's .gitconfig (no init.templateDir) and stays
	// hermetic. A later same-named entry overrides the inherited one for getenv
	// (the same mechanism AGENT_FACTORY_HOME already relies on), so the
	// fixture value wins over any runner-exported HOME/XDG_CONFIG_HOME.
	// GIT_CONFIG_NOSYSTEM=1 makes git (including the `git init` below) skip
	// /etc/gitconfig entirely. filterInheritedEnv only removes GIT_CONFIG_SYSTEM
	// (which points git at a *different* system file but, if unset, leaves the
	// default /etc/gitconfig in force). A runner whose /etc/gitconfig sets
	// init.templateDir would have `git init` copy that template's .git/config —
	// which can carry an empty user.name/user.email — into repoDir, where it
	// overrides the fixture's later $HOME/.gitconfig and makes checkGitIdentity
	// actionable for a runner-config reason rather than the regression under test.
	// Disabling the system config keeps the init hermetic to the fixture's global
	// config alone.
	childEnv := append(
		filterInheritedEnv(os.Environ()),
		"AGENT_FACTORY_HOME="+home,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		// Pin SHELL to a fixture-safe /bin/sh so the spawned af's Claude
		// shell probe (config/claude_probe.go GetClaudeCommand) takes the
		// else-branch and runs `sh -c "which claude"` instead of spawning an
		// interactive bash/zsh that sources the runner's rc. filterInheritedEnv
		// drops the ZDOTDIR/BASH_ENV/ENV startup overrides that would redirect
		// even a non-interactive shell at the runner's files, so the probe and
		// any other shell the child spawns resolve startup from the fixture HOME.
		"SHELL=/bin/sh",
	)
	initCmd := exec.Command("git", "-C", repoDir, "init", "-q")
	initCmd.Env = childEnv
	if err := initCmd.Run(); err != nil {
		t.Skipf("git unavailable: cannot initialize a throwaway repo for checkGit: %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoDir
	// childEnv is the same filtered, fixture-scoped environment built above for
	// `git init`: the spawned af inherits it so checkGit/checkGitIdentity read
	// the fixture's $HOME/.gitconfig, checkGit resolves repoDir (not an
	// inherited GIT_DIR), and the --setup daemon check resolves the systemd user
	// dir from the fixture's $XDG_CONFIG_HOME. See the childEnv comment above
	// for the per-variable rationale.
	cmd.Env = childEnv
	// Inherit the real PATH (do NOT empty it): checkGit needs git on PATH and a
	// repo CWD (cmd.Dir above), checkTmux needs tmux on PATH, and
	// program_overrides.claude="true" needs /bin/true on PATH. Emptying PATH
	// turns each of these into an actionable FAIL (exit 1) — the trick
	// doctorcmd_exit_test.go uses on purpose.
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("af %v failed to run: %v (stderr: %s)", args, err, errBuf.String())
		}
	}
	return outBuf.Bytes(), errBuf.Bytes(), exitCode
}

// TestDoctorJSONExitZeroSuppressesLogHintWhenDirty is the success-path
// counterpart of TestDoctorJSONExitNonZeroSuppressesLogHintWhenDirty
// (doctorcmd_exit_test.go): it pins the contract that a dirty `af doctor
// --json` run that EXITS 0 leaves real process stderr empty — the same
// contract the non-zero exit path already honors via log.CloseQuiet and that
// api/jsonOut pins for its own --json success path (#3169).
//
// The bug: RunE's success path closed the log with a plain non-mode-aware
// `defer log.Close()`, so a dirty --json run that returned nil (exit 0)
// triggered the deferred close, which printed the human "wrote logs to <path>"
// hint to os.Stderr (log/log.go:565) — a free-form line after the success
// envelope on a 0 exit. The non-zero exit path was already mode-aware
// (log.CloseQuiet under doctorJSONFlag); the success path was missed.
//
// The fix makes the deferred close mode-aware (log.CloseQuiet under --json,
// log.Close otherwise), matching the os.Exit path and the api/jsonOut
// precedent. This test fails before the fix (stderr is exactly
// "wrote logs to <home>/agent-factory.log\n") and passes after.
//
// The `--setup` profile is used because it is hermetic to the machine-wide
// sweeps the default profile performs (orphaned processes, leaked tmux
// sessions, stale temp homes, foreign daemons, ...): those scan the real
// machine and could surface an actionable finding on a busy dev box, making
// the exit-0 assertion fail for an unrelated reason. --setup checks are
// scoped to the AF home, config, git, tmux, agent programs, and daemon
// presence, all of which this fixture controls or guards.
func TestDoctorJSONExitZeroSuppressesLogHintWhenDirty(t *testing.T) {
	requireTmuxOrSkip(t)

	// SocketTempDir, not t.TempDir: the --setup profile's daemon check resolves
	// the daemon control socket under AGENT_FACTORY_HOME, and on macOS a
	// t.TempDir() home is ~107 bytes — past this platform's 103-byte sun_path
	// limit — so checkDaemonHealth reports an actionable FAIL ("cannot resolve
	// daemon socket path: ... is N bytes, over this platform's 103-byte limit")
	// and the run exits 1 instead of 0. The real home (~/.agent-factory) is
	// short, and so is /tmp. See testguard.SocketTempDir and daemonstatuscmd_test.
	home := testguard.SocketTempDir(t)
	// AGENT_FACTORY_HOME and HOME both point at the throwaway home so the
	// config file, the log file, and git's global config (~/.gitconfig) all
	// live inside it.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(doctorDirtySuccessConfig), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(doctorSetupGitIdentity), 0o644))

	stdout, stderr, exitCode := runDoctorSuccessSubprocess(t, home, "doctor", "--setup", "--json")

	// A resolvable default agent plus a warn-only unknown key is exit 0.
	require.Equal(t, 0, exitCode,
		"a --setup run with default_program resolving to /bin/true and only a warn-only unknown key must exit 0, got %d\nstdout: %s\nstderr: %s",
		exitCode, stdout, stderr)

	// The contract this bug breaks: a dirty --json SUCCESS leaves stderr
	// empty so a consumer that merges streams (`af doctor --json 2>&1`) or
	// treats any non-empty stderr on a 0 exit as failure never sees a
	// free-form line after the envelope. The envelope lives on stdout, so a
	// stdout-only consumer is unaffected; this guards the merged-stream one.
	assert.Empty(t, stderr,
		"BUG: dirty --json success leaked a free-form line to stderr (must stay empty); got: %q", stderr)
	assert.NotContains(t, string(stderr), "wrote logs to ",
		"BUG: dirty --json success leaked the wrote-logs hint to stderr; got: %q", stderr)

	// stdout is the shared {data,error} success envelope: the checks are
	// data, and the error is null (mirrors the envelope shape
	// TestDoctorJSONExitNonZeroSuppressesLogHintWhenDirty asserts, but on the
	// success path so error is null).
	var env struct {
		Data  doctor.JSONReport       `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env),
		"stdout must be the shared {data,error} envelope on a --json success; got: %s", stdout)
	assert.Nil(t, env.Error, "a successful --json run has a null error; got: %+v", env.Error)
	assert.NotEmpty(t, env.Data.Checks, "the success envelope must carry the setup checks")

	// dirty was GENUINELY true: the unknown-key WARNING was written to this
	// home's log file. This is what makes CloseQuiet meaningful suppression
	// rather than a no-op over a clean run (dirty=false suppresses nothing
	// anyway), and it guards that the deferred close ran (the log file exists
	// and the warning survived in it, O_APPEND, no buffer to flush).
	logBytes, err := os.ReadFile(filepath.Join(home, "agent-factory.log"))
	require.NoError(t, err, "dirty --json run must write its log file at %s", filepath.Join(home, "agent-factory.log"))
	assert.Contains(t, string(logBytes), `unknown key "unknown_key_in_config"`,
		"the log file must record the config parse warning that set dirty=true, so CloseQuiet is genuine suppression")
}
