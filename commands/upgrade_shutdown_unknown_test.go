package commands

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// RequestShutdown returning ShutdownNoDaemon ALONGSIDE an error means it could
// not determine whether a daemon is running — a socket it could not stat, say.
// That is a third state, and it was being reported as restartPhaseShutdown,
// whose message asserts the daemon "is still running the old binary" (#3097).
//
// Both of the existing remedies are wrong for it: hunting a process that may not
// exist, or waiting for one that is already gone.
func TestRestartDaemon_NoDaemonWithAnErrorIsIndeterminate(t *testing.T) {
	prev := requestDaemonShutdownFn
	t.Cleanup(func() { requestDaemonShutdownFn = prev })
	probeErr := errors.New("stat /run/af.sock: permission denied")
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownNoDaemon, probeErr
	}

	outcome, err := restartDaemonFromPathDetailed("/usr/local/bin/af")

	require.Error(t, err)
	assert.Equal(t, restartPhaseShutdownUnknown, outcome.FailedPhase,
		"a shutdown CHECK that failed establishes nothing; reporting it as a failed STOP asserts a "+
			"running daemon that was never observed")
	assert.ErrorIs(t, err, probeErr)
	assert.NotContains(t, err.Error(), "failed to stop",
		"nothing was established about a running daemon, so nothing may claim one refused to stop")
}

// The message must state the uncertainty and offer a CHECK rather than a
// remedy — a remedy presumes the state the check could not establish.
func TestReportUpgradeRestart_IndeterminateShutdownSaysSo(t *testing.T) {
	var out, errOut bytes.Buffer
	outcome := restartOutcome{
		Shutdown:    daemon.ShutdownNoDaemon,
		FailedPhase: restartPhaseShutdownUnknown,
	}

	reportUpgradeRestart(&out, &errOut, outcome, errors.New("permission denied"), "/usr/local/bin/af")

	assert.Contains(t, out.String(), "Upgraded successfully!",
		"the binary really was written; the uncertainty is only about the daemon")
	msg := errOut.String()
	assert.Contains(t, msg, "Could not determine")
	assert.Contains(t, msg, "af daemon status", "an unknown state earns a check, not a remedy")
	assert.False(t, strings.Contains(msg, "It is still running the old binary"),
		"that sentence asserts a daemon nobody observed — the whole defect")
}
