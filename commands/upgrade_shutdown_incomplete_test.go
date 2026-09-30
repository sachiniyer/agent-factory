package commands

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// #5007: the old daemon acknowledged Shutdown but was still alive when the
// wait's bound expired. Respawning then races it — the replacement can take
// the draining daemon for a live one, skip the spawn, and leave nothing
// running — so the respawn is withheld, and the report says the old daemon is
// still finishing its shutdown rather than that it refused to stop or that
// nothing is running.

// TestRespawnAfterUpgradeWithheldWhenShutdownIncomplete: a wait that expires
// with ErrShutdownIncomplete returns before the unit/ad-hoc split, so NEITHER
// respawn path runs.
func TestRespawnAfterUpgradeWithheldWhenShutdownIncomplete(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("unit installed=%v", installed), func(t *testing.T) {
			restartCalls, ensureCalls := stubRespawnCollaborators(t, installed, nil)
			var waitedPID int
			waitForShutdownCompletionFn = func(pid int) error {
				waitedPID = pid
				return fmt.Errorf("%w: daemon pid %d still running", daemon.ErrShutdownIncomplete, pid)
			}

			_, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 4242)
			if !errors.Is(err, daemon.ErrShutdownIncomplete) {
				t.Fatalf("respawnDaemonAfterUpgrade error = %v, want it to wrap daemon.ErrShutdownIncomplete", err)
			}
			if waitedPID != 4242 {
				t.Fatalf("wait got pid %d, want the old daemon's 4242", waitedPID)
			}
			if *restartCalls != 0 || *ensureCalls != 0 {
				t.Fatalf("unit restarts = %d, ad-hoc spawns = %d; want both 0 — the respawn must be withheld while the old daemon is alive",
					*restartCalls, *ensureCalls)
			}
		})
	}
}

// TestRestartDetailedReportsShutdownIncomplete: a withheld respawn is its own
// phase, carrying the old daemon's PID for the report.
func TestRestartDetailedReportsShutdownIncomplete(t *testing.T) {
	prevShutdown, prevRespawn := requestDaemonShutdownFn, respawnDaemonFn
	t.Cleanup(func() { requestDaemonShutdownFn, respawnDaemonFn = prevShutdown, prevRespawn })

	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
		return daemon.ShutdownViaRPC, 4242, nil
	}
	var respawnPID int
	respawnDaemonFn = func(_ string, pid int) (respawnResult, error) {
		respawnPID = pid
		return respawnResult{}, fmt.Errorf("the old daemon is still finishing (%w)", daemon.ErrShutdownIncomplete)
	}

	outcome, err := restartDaemonFromPathDetailed(testUpgradeDaemonPath)
	if !errors.Is(err, daemon.ErrShutdownIncomplete) {
		t.Fatalf("error = %v, want it to wrap daemon.ErrShutdownIncomplete", err)
	}
	if outcome.FailedPhase != restartPhaseShutdownIncomplete {
		t.Fatalf("FailedPhase = %v, want restartPhaseShutdownIncomplete", outcome.FailedPhase)
	}
	if outcome.OldPID != 4242 || respawnPID != 4242 {
		t.Fatalf("OldPID = %d, respawn got pid %d; want both 4242", outcome.OldPID, respawnPID)
	}
	if outcome.Respawned {
		t.Fatalf("Respawned = true for a withheld respawn")
	}
}

// TestReportUpgradeRestartShutdownIncomplete pins the exact #5007 wording, with
// and without a known PID.
func TestReportUpgradeRestartShutdownIncomplete(t *testing.T) {
	for _, tc := range []struct {
		pid  int
		want string
	}{
		{
			pid: 4242,
			want: "The old daemon is still finishing its shutdown — it normally exits on its own, but if it persists it may be wedged: " +
				"`ps -p 4242` / `kill -9 4242`" + "; then run af again.\n",
		},
		{
			pid: 0,
			want: "The old daemon is still finishing its shutdown — it normally exits on its own, but if it persists it may be wedged: " +
				"look for a leftover `af --daemon` and `kill -9` it; then run af again.\n",
		},
	} {
		t.Run(fmt.Sprintf("pid=%d", tc.pid), func(t *testing.T) {
			var out, errOut bytes.Buffer
			outcome := restartOutcome{
				Shutdown:    daemon.ShutdownViaRPC,
				FailedPhase: restartPhaseShutdownIncomplete,
				OldPID:      tc.pid,
			}
			reportUpgradeRestart(&out, &errOut, outcome, errors.New("withheld"), testUpgradeDaemonPath)

			if got := out.String(); got != "Upgraded successfully!\n" {
				t.Fatalf("stdout = %q, want %q", got, "Upgraded successfully!\n")
			}
			if got := errOut.String(); got != tc.want {
				t.Fatalf("stderr = %q\nwant     %q", got, tc.want)
			}
		})
	}
}
