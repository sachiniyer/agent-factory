package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/doctor"

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

// runDoctorSuccessSubprocess runs the built af binary with `af doctor
// --setup --json` in a throwaway home the caller has already seeded, under a
// real PATH so the setup prerequisites (git + the in-repo CWD for checkGit,
// tmux for checkTmux, /bin/true for the program_overrides.claude override)
// resolve to PASS. The af binary runs by absolute path from afTestBinary, so
// it does not need PATH to locate itself. Returns stdout, stderr, and the
// process exit code. This is the exit-0 counterpart of runDoctorSubprocess in
// doctorcmd_exit_test.go, which empties PATH to FORCE exit 1; this one needs
// exit 0, so PATH must resolve the setup prerequisites.
func runDoctorSuccessSubprocess(t *testing.T, home string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	bin := afTestBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"AGENT_FACTORY_HOME="+home,
		"HOME="+home,
	)
	// Inherit the real PATH (do NOT empty it): checkGit needs git on PATH and a
	// repo CWD (the test binary's CWD is the commands/ package inside the repo),
	// checkTmux needs tmux on PATH, and program_overrides.claude="true" needs
	// /bin/true on PATH. Emptying PATH turns each of these into an actionable
	// FAIL (exit 1) — the trick doctorcmd_exit_test.go uses on purpose.
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

	home := t.TempDir()
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
