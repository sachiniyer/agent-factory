package commands

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// A shutdown that was acknowledged but had not finished at the wait's bound
// leaves the old daemon ALIVE — it may still be joining durable work — so the
// respawn is withheld (#5007). Reporting that as restartPhaseRespawn claims the
// old daemon is gone and nothing is running, which is false. The phase must be
// carried from daemon.ErrShutdownIncomplete, and every other respawn failure
// must stay restartPhaseRespawn.
func TestRespawn_Draining_BoundIsShutdownIncompletePhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		waitErr   error
		wantPhase restartPhase
	}{
		{
			name:      "unfinished shutdown",
			waitErr:   fmt.Errorf("%w: daemon pid 4242 still running 1m0s after shutdown was acknowledged", daemon.ErrShutdownIncomplete),
			wantPhase: restartPhaseShutdownIncomplete,
		},
		{
			name:      "any other respawn failure",
			waitErr:   errors.New("something else went wrong"),
			wantPhase: restartPhaseRespawn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRespawnCollaborators(t, false, nil)
			prevShutdown, prevRespawn := requestDaemonShutdownFn, respawnDaemonFn
			t.Cleanup(func() {
				requestDaemonShutdownFn = prevShutdown
				respawnDaemonFn = prevRespawn
			})
			requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
				return daemon.ShutdownViaRPC, 4242, nil
			}
			respawnDaemonFn = respawnDaemonAfterUpgrade
			waitForShutdownCompletionFn = func(int) error { return tc.waitErr }

			outcome, err := restartDaemonFromPathDetailed("/usr/local/bin/af")

			require.Error(t, err)
			assert.ErrorIs(t, err, tc.waitErr)
			assert.Equal(t, tc.wantPhase, outcome.FailedPhase)
			assert.False(t, outcome.Respawned)
		})
	}
}

// The report must say the true thing: the old daemon is still finishing its
// shutdown — never "No daemon is running", and never the start-a-daemon hint,
// which would race it for the home lock. Nor may it promise the daemon exits:
// a wedged one may not. The wording is fixed verbatim by the #5007 spec, in
// both the pid-known and pid-unknown forms.
func TestUpgradeReport_Draining_BoundWording(t *testing.T) {
	// Never probe the host's real daemon if a regression reaches the health
	// fallback: answer as if nothing is verifiable.
	prevHealth := daemonHealthFn
	t.Cleanup(func() { daemonHealthFn = prevHealth })
	daemonHealthFn = func() daemon.HealthStatus {
		return daemon.HealthStatus{PingErr: errors.New("dial: no such file or directory")}
	}

	for _, tc := range []struct {
		name     string
		oldPID   int
		wantLine string
	}{
		{
			name:     "pid known",
			oldPID:   4242,
			wantLine: "The old daemon is still finishing its shutdown — it normally exits on its own, but if it persists it may be wedged: `ps -p 4242` / `kill 4242`; then run af again.",
		},
		{
			name:     "pid unknown",
			oldPID:   0,
			wantLine: "The old daemon is still finishing its shutdown — it normally exits on its own, but if it persists it may be wedged: look for a leftover `af --daemon`; then run af again.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			outcome := restartOutcome{
				Shutdown:    daemon.ShutdownViaRPC,
				FailedPhase: restartPhaseShutdownIncomplete,
				OldPID:      tc.oldPID,
			}
			restartErr := fmt.Errorf("failed to restart daemon: %w", daemon.ErrShutdownIncomplete)

			reportUpgradeRestart(&out, &errOut, outcome, restartErr, "/usr/local/bin/af")

			assert.Equal(t, "Upgraded successfully!\n", out.String())
			assert.Equal(t, tc.wantLine+"\nDetail: "+restartErr.Error()+"\n", errOut.String(),
				"the bound-expired wording is fixed by the #5007 spec")
			msg := errOut.String()
			assert.NotContains(t, msg, "No daemon is running", "the old daemon is alive; saying otherwise is the defect")
			assert.NotContains(t, msg, startDaemonHint(), "starting a daemon now would race the draining one")
		})
	}
}
