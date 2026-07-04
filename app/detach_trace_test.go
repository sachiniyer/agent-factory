package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// captureWarningLog redirects log.WarningLog into a buffer for the duration
// of the test and restores the previous writer on cleanup. Tests using it
// must not run in parallel — WarningLog is package-global.
func captureWarningLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&buf)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })
	return &buf
}

// setDetachTraceEnabled overrides the marker gate for one test and restores
// the prior value on cleanup.
func setDetachTraceEnabled(t *testing.T, enabled bool) {
	t.Helper()
	prev := detachTraceEnabled
	detachTraceEnabled = enabled
	t.Cleanup(func() { detachTraceEnabled = prev })
}

// TestDetachTraceEnabledFromEnv covers the AF_DETACH_TRACE parsing that
// initializes detachTraceEnabled at package init (#788): off unless the
// variable is exactly "1".
func TestDetachTraceEnabledFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"1", true},
	} {
		t.Setenv("AF_DETACH_TRACE", tc.value)
		require.Equal(t, tc.want, detachTraceEnabledFromEnv(),
			"AF_DETACH_TRACE=%q", tc.value)
	}
}

// TestDetachTraceMarkers_SilentWhenDisabled asserts the steady-state #788
// behavior: with the gate off (the default), none of the marker helpers
// write anything to the warning log.
func TestDetachTraceMarkers_SilentWhenDisabled(t *testing.T) {
	buf := captureWarningLog(t)
	setDetachTraceEnabled(t, false)

	start := time.Now()
	detachTrace(start, "marker-a")
	detachTraceMark("marker-c")

	require.Empty(t, buf.String(),
		"[detach-trace] markers must not be emitted when AF_DETACH_TRACE is unset")
}

// TestDetachTraceMarkers_EmitWhenEnabled asserts the opt-in path: with the
// gate on, every marker helper writes a [detach-trace] line.
func TestDetachTraceMarkers_EmitWhenEnabled(t *testing.T) {
	buf := captureWarningLog(t)
	setDetachTraceEnabled(t, true)

	start := time.Now()
	detachTrace(start, "marker-a")
	detachTraceMark("marker-c")

	out := buf.String()
	require.Equal(t, 2, strings.Count(out, "[detach-trace]"),
		"each marker helper must emit exactly one [detach-trace] line:\n%s", out)
	require.Contains(t, out, "marker-a")
	require.Contains(t, out, "marker-c")
}

// TestSlowDetachDump_HintsAtEnvVar asserts the SLOW DETACH log line points
// users at AF_DETACH_TRACE=1 when the marker gate is off — that hint is how
// someone who hits a slow detach in the wild learns to capture step-level
// traces (#788) — and omits the hint when tracing is already on.
func TestSlowDetachDump_HintsAtEnvVar(t *testing.T) {
	// newTestHome points the config dir at a tempdir so the dump file from
	// dumpSlowDetach lands somewhere disposable.
	newTestHome(t)

	buf := captureWarningLog(t)

	setDetachTraceEnabled(t, false)
	dumpSlowDetach("hint-test-disabled", time.Now())
	require.Contains(t, buf.String(), "re-run with AF_DETACH_TRACE=1 for step-level tracing",
		"slow-detach log line must carry the tracing hint when the gate is off")

	buf.Reset()
	setDetachTraceEnabled(t, true)
	dumpSlowDetach("hint-test-enabled", time.Now())
	require.Contains(t, buf.String(), "SLOW DETACH (hint-test-enabled)")
	require.NotContains(t, buf.String(), "re-run with AF_DETACH_TRACE=1",
		"hint is redundant when tracing is already enabled")
}
