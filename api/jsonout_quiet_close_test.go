package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// TestJSONOut_EnvelopeDirtySuccessLeavesStderrClean pins the contract the
// warnWriter comment in api.go states for the --json success path (#3169):
// under --json a SUCCESS must leave stderr empty, so consumers that merge
// stderr into the parsed stream (af ... --json 2>&1) or treat any non-empty
// stderr as failure never see a free-form line after the envelope.
//
// Before the fix, jsonOut's envelope branch (unlike jsonError's) did not call
// log.CloseQuiet(), so a dirty --json success — stdout already carries the
// envelope while a WARNING/ERROR earlier in the run set log.dirty — left the
// command's deferred log.Close() free to print "wrote logs to ..." on stderr
// after the envelope went out. This test drives that exact interaction:
// Initialize (open a log file), dirty the run through the production
// dirtyWriter (NOT a redirected buffer), call jsonOut under --json, then the
// reporting log.Close() every api RunE defers.
func TestJSONOut_EnvelopeDirtySuccessLeavesStderrClean(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	log.Initialize(false)
	// Safety net: CloseQuiet never prints, so even a panic before jsonOut
	// cannot leak "wrote logs to" on real stderr, and it reroutes the loggers
	// off the captured stderr pipe once the test unwinds.
	t.Cleanup(log.CloseQuiet)

	// Dirty the run the way a degraded-but-successful command does: a WARNING
	// through the production dirtyWriter (the sink Initialize wraps
	// WarningLog/ErrorLog in, NOT a buffer that captureWarnings would
	// substitute) flips log.dirty, which a reporting log.Close() turns into
	// "wrote logs to ..." on stderr (#1749).
	log.WarningLog.Printf("degraded-but-successful run marked dirty")

	var stdout, stderr string
	stderr = captureStderr(t, func() {
		withEnvelope(t, func() {
			stdout = captureStdout(t, func() {
				require.NoError(t, jsonOut(map[string]bool{"ok": true}))
				// The command's deferred log.Close() — the reporting close
				// every api RunE registers — fires after jsonOut returns. On
				// the unfixed success path this prints "wrote logs to ..." to
				// stderr after the envelope went out on stdout.
				log.Close()
			})
		})
	})

	// stdout carries the success envelope: data present, error absent.
	var env struct {
		Data  json.RawMessage  `json:"data"`
		Error *json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout must be the --json envelope; got %q", stdout)
	require.Nil(t, env.Error, "this is a SUCCESS: error must be null; got %q", stdout)
	require.JSONEq(t, `{"ok":true}`, string(env.Data))

	// The contract: a dirty --json success must not leak the log location on
	// stderr, and stderr must be empty altogether.
	require.False(t, strings.Contains(stderr, "wrote logs to"),
		"dirty --json success leaked the log location on stderr: %q", stderr)
	require.Empty(t, stderr, "stderr must be empty under --json success; got %q", stderr)
}

// TestJSONOut_BareDirtySuccessStillReportsLogLocation guards that the fix only
// quieted the --json path: the bare (human) output path still points the user
// at the log on a dirty success (#1749), so the quiet-close did not silently
// swallow the log-location note everywhere. This must hold both before and
// after the fix.
func TestJSONOut_BareDirtySuccessStillReportsLogLocation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	log.Initialize(false)
	t.Cleanup(log.CloseQuiet)

	origEnvelope := envelopeOutput
	envelopeOutput = false
	t.Cleanup(func() { envelopeOutput = origEnvelope })

	log.WarningLog.Printf("dirty the bare run too")

	var stderr string
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			require.NoError(t, jsonOut(map[string]bool{"ok": true}))
			// The bare path's deferred reporting close still prints the note.
			log.Close()
		})
	})

	require.True(t, strings.Contains(stderr, "wrote logs to"),
		"bare dirty success must still report the log location (#1749); got %q", stderr)
}
