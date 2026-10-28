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
// lives on the os.Exit path of doctorCmd.RunE, and os.Exit terminates the
// test binary if driven in-process (doctorcmd_test.go:43-44 spells out why the
// in-process tests keep doctorExitCode at 0). They reuse the package-level
// afTestBinary seam (built once per test binary by sshrelaycmd_test.go), the
// same subprocess pattern the ssh-relay tests use to ask "what does the whole
// program write to fd 1/2".

// doctorDirtyConfig is a config that deterministically sets dirty=true: the
// unknown top-level key makes config_parse.go: warnUnknownTomlKeys emit a
// log.WarningLog (log/log.go dirtyWriter -> dirty.Store(true)), and the load
// itself succeeds (unknown keys are warned-and-ignored, not errored). It pins
// default_program=claude so a missing claude binary resolves to an actionable
// FAIL (UnresolvedCount > 0 -> doctorExitCode 1) rather than an advisory WARN.
const doctorDirtyConfig = `default_program = "claude"
unknown_key_in_config = "will-warn"
`

// writeDoctorConfig writes doctorDirtyConfig into home as config.toml.
func writeDoctorConfig(t *testing.T, home string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(doctorDirtyConfig), 0o600))
}

// runDoctorSubprocess runs the built af binary with the given `af doctor`
// args in a throwaway home the caller has already seeded, under a PATH that
// holds no executable so no agent (claude above all) is findable: the claude
// probe's LookPath falls through to the missing-claude warning (dirty=true)
// and the default-program check reports an actionable FAIL (exit 1). The af
// binary runs by absolute path from afTestBinary, so it does not need PATH.
// Returns stdout, stderr, and the process exit code.
func runDoctorSubprocess(t *testing.T, home string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	bin := afTestBinary(t)
	// A PATH dir that exists but holds no executable: LookPath for every
	// agent (and tmux, git, ...) fails fast, so checkAgentBinaries reports
	// claude (the configured default) as an actionable FAIL.
	emptyPathDir := t.TempDir()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"AGENT_FACTORY_HOME="+home,
		"HOME="+home,
		"PATH="+emptyPathDir,
	)
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

// TestDoctorPlainExitNonZeroWritesLogHintWhenDirty is the previously-failing
// test for the bug: in plain mode, a doctor run that exits non-zero
// (actionable finding -> doctorExitCode 1) AND recorded a WARNING (dirty=true)
// must print the "wrote logs to <path>" hint to stderr before exiting. Before
// the fix, os.Exit at doctorcmd.go skipped the deferred log.Close(), so the
// hint was lost and an operator saw exit 1 with empty stderr and no pointer to
// the log file the report did not surface.
func TestDoctorPlainExitNonZeroWritesLogHintWhenDirty(t *testing.T) {
	home := t.TempDir()
	writeDoctorConfig(t, home)

	stdout, stderr, exitCode := runDoctorSubprocess(t, home, "doctor")

	// The default program (claude) is missing under the empty PATH, so the
	// run has an unresolved actionable finding -> doctorExitCode 1.
	require.Equal(t, 1, exitCode, "a missing configured agent must exit 1, got %d (stderr: %q)", exitCode, stderr)

	// The fix: the plain-mode "wrote logs to <path>" hint reaches stderr.
	require.NotEmpty(t, stderr, "stderr must carry the wrote-logs hint on a dirty non-zero exit; got empty stderr")
	assert.Contains(t, string(stderr), "wrote logs to ")
	// The hint names the log file this home actually wrote to, so the
	// operator can open it.
	assert.Contains(t, string(stderr), filepath.Join(home, "agent-factory.log"))

	// stdout still carries the rendered report unchanged; the hint is
	// stderr-only, so the report bytes are not perturbed.
	assert.Contains(t, string(stdout), "Agent Factory Doctor")
	assert.Contains(t, string(stdout), "claude")

	// Sanity: the log file the hint points at genuinely contains a WARNING
	// (dirty was really set, so the hint is correct-by-gate, not spurious).
	// This also guards the claim that log content survives (O_APPEND, no
	// buffer to flush), so the pointer is worth following.
	logBytes, err := os.ReadFile(filepath.Join(home, "agent-factory.log"))
	require.NoError(t, err, "the hinted log file must exist at %s", filepath.Join(home, "agent-factory.log"))
	assert.Contains(t, string(logBytes), `unknown key "unknown_key_in_config"`,
		"the log file must record the config parse warning that set dirty=true")
}

// TestDoctorJSONExitNonZeroSuppressesLogHintWhenDirty is the mode-awareness
// guard: in --json mode, stderr must stay machine-parseable, so the hint is
// suppressed via log.CloseQuiet() (mirroring jsonWrapError). A naive
// log.Close() before os.Exit, without the doctorJSONFlag branch, would pollute
// JSON-mode stderr with a human-readable line and break the {data,error}
// contract — this test pins the branch.
func TestDoctorJSONExitNonZeroSuppressesLogHintWhenDirty(t *testing.T) {
	home := t.TempDir()
	writeDoctorConfig(t, home)

	stdout, stderr, exitCode := runDoctorSubprocess(t, home, "doctor", "--json")

	require.Equal(t, 1, exitCode, "a missing configured agent must exit 1 in JSON mode too, got %d", exitCode)
	// The whole point of the mode branch: stderr stays empty in JSON mode
	// even when dirty=true (which would print the hint in plain mode).
	assert.Empty(t, stderr, "JSON-mode stderr must stay empty (no wrote-logs hint); got: %q", stderr)

	// dirty was genuinely true in JSON mode too: the same config parse
	// warning that set it in plain mode was written to this home's log
	// file. This is what makes the doctorJSONFlag branch meaningful —
	// log.CloseQuiet is suppressing a hint that would otherwise print, not
	// running over a clean run where dirty is false.
	logBytes, err := os.ReadFile(filepath.Join(home, "agent-factory.log"))
	require.NoError(t, err, "JSON-mode run must still write its log file at %s", filepath.Join(home, "agent-factory.log"))
	assert.Contains(t, string(logBytes), `unknown key "unknown_key_in_config"`,
		"the log file must record the config parse warning that set dirty=true, so CloseQuiet is genuine suppression")

	// stdout is the {data,error} envelope. Findings are data, not an error:
	// doctor.RenderJSON emits apiproto.Success(BuildJSONReport(...)), so the
	// envelope carries the report (with summary.unresolved > 0) in data and a
	// null error — and then RunE exits 1 from doctorExitCode. The exit code
	// comes from the findings, not from a JSON error envelope.
	var env struct {
		Data  doctor.JSONReport       `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env), "stdout must be the shared {data,error} envelope in --json mode; got: %s", stdout)
	assert.Nil(t, env.Error, "findings are data, not a JSON error envelope")
	assert.NotEmpty(t, env.Data.Checks, "the report envelope must carry the checks")
	assert.True(t, env.Data.Summary.Unresolved > 0,
		"the actionable missing-agent finding must surface in summary.unresolved, got %d", env.Data.Summary.Unresolved)
}
