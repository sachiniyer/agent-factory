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

// filterInheritedGitEnv strips Git's environment from the inherited process
// environment so the child git and the spawned af read the fixture's
// $HOME/.gitconfig as the global config (and /etc/gitconfig as the system
// config), operate against the fixture's repoDir (cmd.Dir), and are not
// redirected at an external repository or identity the test runner pointed at.
//
// Two shapes of entry are dropped, mirroring the override classification in
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
//     they cannot be blanked.
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
func filterInheritedGitEnv(env []string) []string {
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
			"GIT_COMMON_DIR":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
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
	// Filter the inherited Git environment for the `git init` itself too: if a
	// caller exports GIT_DIR / GIT_WORK_TREE (e.g. running from a Git hook),
	// `git -C repoDir init` would initialize or reuse that external repository
	// rather than repoDir. filterInheritedGitEnv strips the repo-local variables
	// so the init targets repoDir and the fixture stays hermetic.
	initCmd := exec.Command("git", "-C", repoDir, "init", "-q")
	initCmd.Env = filterInheritedGitEnv(os.Environ())
	if err := initCmd.Run(); err != nil {
		t.Skipf("git unavailable: cannot initialize a throwaway repo for checkGit: %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoDir
	// Strip inherited Git environment the test runner may export:
	//   - Git config overrides: a GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM file
	//     (e.g. a config without a user identity), or a GIT_CONFIG_COUNT /
	//     GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n command-line set (e.g. empty
	//     user.name / user.email values). With any of these set, git ignores or
	//     layers on top of $HOME/.gitconfig, so checkGitIdentity reads the
	//     runner's identity instead of the fixture identity this test seeds
	//     below and reports an actionable git-identity finding — failing the
	//     exit-0 assertion for the runner's configuration rather than the
	//     behavior under test.
	//   - Git repository-local variables (GIT_DIR, GIT_WORK_TREE, ...): if a
	//     caller exports these, the spawned af inherits them and checkGit
	//     (`git rev-parse --show-toplevel`) probes the caller's repository
	//     instead of repoDir — again breaking the exit-0 assertion for a
	//     reason unrelated to the regression under test.
	// An empty value still overrides (git treats an empty GIT_CONFIG_GLOBAL as
	// "no global file", an empty GIT_DIR still points at the current directory,
	// and an empty indexed value still applies), so the entries are dropped
	// entirely, not cleared (filterInheritedGitEnv). With them gone, HOME=home
	// makes git read home/.gitconfig as the global config and cmd.Dir makes
	// checkGit resolve repoDir, so the seeded user.name/user.email resolve and
	// checkGitIdentity passes.
	cmd.Env = append(
		filterInheritedGitEnv(os.Environ()),
		"AGENT_FACTORY_HOME="+home,
		"HOME="+home,
	)
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
