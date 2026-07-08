package api

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureStderr mirrors captureStdout (tasks_test.go) for the error path.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

// withEnvelope flips the opt-in --json flag on for the duration of fn and
// restores it, so a test can exercise the enveloped output path in isolation.
func withEnvelope(t *testing.T, fn func()) {
	t.Helper()
	orig := envelopeOutput
	envelopeOutput = true
	t.Cleanup(func() { envelopeOutput = orig })
	fn()
}

// TestJSONOut_DefaultIsBarePayload locks in that the default (no --json) output
// is the bare, byte-for-byte payload — the additive contract that keeps existing
// scripts parsing `af sessions ...` output working after #1029.
func TestJSONOut_DefaultIsBarePayload(t *testing.T) {
	require.False(t, envelopeOutput, "envelope must default OFF")

	out := captureStdout(t, func() {
		require.NoError(t, jsonOut(map[string]bool{"ok": true}))
	})
	require.Equal(t, "{\n  \"ok\": true\n}\n", out)
}

// TestJSONOut_EnvelopeWrapsPayload verifies the opt-in path wraps the same
// payload in the success envelope.
func TestJSONOut_EnvelopeWrapsPayload(t *testing.T) {
	var out string
	withEnvelope(t, func() {
		out = captureStdout(t, func() {
			require.NoError(t, jsonOut(map[string]bool{"ok": true}))
		})
	})
	require.JSONEq(t, `{"data":{"ok":true},"error":null}`, out)
}

// TestJSONError_DefaultIsBareError locks in the historical stderr shape
// (compact {"error":"<msg>"}) and that the original error is returned unchanged.
func TestJSONError_DefaultIsBareError(t *testing.T) {
	require.False(t, envelopeOutput, "envelope must default OFF")

	sentinel := errors.New("nope")
	var ret error
	out := captureStderr(t, func() {
		ret = jsonError(sentinel)
	})
	require.Equal(t, sentinel, ret)
	require.Equal(t, "{\"error\":\"nope\"}\n", out)
}

// TestJSONError_EnvelopeWrapsError verifies the opt-in path emits the failure
// envelope to stderr while still returning the original error (exit code stays).
func TestJSONError_EnvelopeWrapsError(t *testing.T) {
	sentinel := errors.New("nope")
	var ret error
	var out string
	SessionsCmd.SilenceUsage = false
	SessionsCmd.SilenceErrors = false
	TasksCmd.SilenceUsage = false
	TasksCmd.SilenceErrors = false
	t.Cleanup(func() {
		SessionsCmd.SilenceUsage = false
		SessionsCmd.SilenceErrors = false
		TasksCmd.SilenceUsage = false
		TasksCmd.SilenceErrors = false
	})
	withEnvelope(t, func() {
		out = captureStderr(t, func() {
			ret = jsonError(sentinel)
		})
	})
	require.Equal(t, sentinel, ret)
	require.JSONEq(t, `{"data":null,"error":{"message":"nope"}}`, out)
	require.True(t, SessionsCmd.SilenceUsage)
	require.True(t, SessionsCmd.SilenceErrors)
	require.True(t, TasksCmd.SilenceUsage)
	require.True(t, TasksCmd.SilenceErrors)
}
