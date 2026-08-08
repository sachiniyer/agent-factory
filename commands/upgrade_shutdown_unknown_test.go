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
	prevHealth := daemonHealthFn
	t.Cleanup(func() { daemonHealthFn = prevHealth })
	// Health cannot verify a pid either — genuinely unknown.
	// Nothing answered the ping AND no pid could be verified — genuinely unknown.
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{PingErr: errors.New("dial: no such file or directory")}
	}

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

// A failed shutdown CHECK does not mean nothing knows. Health reads the pid file
// directly, so a socket that could not be statted can still sit beside a VERIFIED
// live pid — and then the state is determined, not unknown.
//
// Reporting uncertainty there would downgrade a determined answer, which is the
// same collapse this change exists to fix, one source over. It would also send the
// operator to `af daemon restart`, which re-enters the very check that failed.
func TestReportUpgradeRestart_IndeterminateButPIDVerifiedReportsRunning(t *testing.T) {
	prevHealth := daemonHealthFn
	t.Cleanup(func() { daemonHealthFn = prevHealth })
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{PIDVerified: true, PIDFilePID: 4242}
	}

	var out, errOut bytes.Buffer
	outcome := restartOutcome{Shutdown: daemon.ShutdownNoDaemon, FailedPhase: restartPhaseShutdownUnknown}

	reportUpgradeRestart(&out, &errOut, outcome, errors.New("permission denied"), "/usr/local/bin/af")

	msg := errOut.String()
	assert.Contains(t, msg, "still running the old binary",
		"a verified live pid IS an answer; reporting it as unknown throws away a determined state")
	assert.Contains(t, msg, "kill 4242",
		"and it earns the real remedy, naming the pid, rather than a check")
	assert.NotContains(t, msg, "Could not determine",
		"uncertainty is only for when the second source cannot answer either")
}

// A daemon that ANSWERED the ping is alive, whatever the pid file says. PingErr
// is nil only when something responded on the control socket, and Health always
// attempts that ping — so a transient stat error can leave PIDVerified false
// while the daemon is demonstrably up. Reporting "unknown" about a daemon that
// just talked to us is the same collapse one layer down.
func TestReportUpgradeRestart_IndeterminateButPingAnsweredReportsRunning(t *testing.T) {
	prevHealth := daemonHealthFn
	t.Cleanup(func() { daemonHealthFn = prevHealth })
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{PingErr: nil} // answered; pid file unreadable
	}

	var out, errOut bytes.Buffer
	outcome := restartOutcome{Shutdown: daemon.ShutdownNoDaemon, FailedPhase: restartPhaseShutdownUnknown}

	reportUpgradeRestart(&out, &errOut, outcome, errors.New("stat: permission denied"), "/usr/local/bin/af")

	msg := errOut.String()
	assert.Contains(t, msg, "still running the old binary",
		"the daemon answered the ping; that is a determined state, not an unknown one")
	assert.NotContains(t, msg, "Could not determine",
		"uncertainty is only for when neither the ping nor the pid can answer")
	assert.Contains(t, msg, "af daemon status",
		"with no verified pid to name, the hint points at the command that finds it")
}
