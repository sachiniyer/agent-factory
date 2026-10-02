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
			old := daemon.ShutdownTarget{PID: 4242, StartToken: "incarnation"}
			var waited daemon.ShutdownTarget
			waitForShutdownCompletionFn = func(target daemon.ShutdownTarget) error {
				waited = target
				return fmt.Errorf("%w: daemon pid %d still running", daemon.ErrShutdownIncomplete, target.PID)
			}

			_, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, old)
			if !errors.Is(err, daemon.ErrShutdownIncomplete) {
				t.Fatalf("respawnDaemonAfterUpgrade error = %v, want it to wrap daemon.ErrShutdownIncomplete", err)
			}
			if waited != old {
				t.Fatalf("wait got target %+v, want the old daemon's %+v", waited, old)
			}
			if *restartCalls != 0 || *ensureCalls != 0 {
				t.Fatalf("unit restarts = %d, ad-hoc spawns = %d; want both 0 — the respawn must be withheld while the old daemon is alive",
					*restartCalls, *ensureCalls)
			}
		})
	}
}

// TestRestartDetailedReportsShutdownIncomplete: a withheld respawn is its own
// phase, carrying the old daemon's PID for the report and its whole target
// (PID and start token) into the wait.
func TestRestartDetailedReportsShutdownIncomplete(t *testing.T) {
	prevShutdown, prevRespawn := requestDaemonShutdownFn, respawnDaemonFn
	t.Cleanup(func() { requestDaemonShutdownFn, respawnDaemonFn = prevShutdown, prevRespawn })

	requestDaemonShutdownFn = func() (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 4242, StartToken: "incarnation"}, nil
	}
	var respawnTarget daemon.ShutdownTarget
	respawnDaemonFn = func(_ string, target daemon.ShutdownTarget) (respawnResult, error) {
		respawnTarget = target
		return respawnResult{}, fmt.Errorf("the old daemon is still finishing (%w)", daemon.ErrShutdownIncomplete)
	}

	outcome, err := restartDaemonFromPathDetailed(testUpgradeDaemonPath)
	if !errors.Is(err, daemon.ErrShutdownIncomplete) {
		t.Fatalf("error = %v, want it to wrap daemon.ErrShutdownIncomplete", err)
	}
	if outcome.FailedPhase != restartPhaseShutdownIncomplete {
		t.Fatalf("FailedPhase = %v, want restartPhaseShutdownIncomplete", outcome.FailedPhase)
	}
	if outcome.OldPID != 4242 {
		t.Fatalf("OldPID = %d, want 4242", outcome.OldPID)
	}
	if want := (daemon.ShutdownTarget{PID: 4242, StartToken: "incarnation"}); respawnTarget != want {
		t.Fatalf("respawn got target %+v, want the shutdown's %+v — the start token must reach the wait", respawnTarget, want)
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
			want: "The old daemon is still finishing its shutdown (pid 4242) — it normally exits on its own: wait a moment and run af again. " +
				"If ps -p 4242 still shows it after several minutes, it may be wedged: kill -9 4242 (in-flight shutdown work may be lost).\n",
		},
		{
			pid: 0,
			want: "The old daemon is still finishing its shutdown — it normally exits on its own: wait a moment and run af again. " +
				"If a leftover `af --daemon` still shows after several minutes, it may be wedged: kill -9 it (in-flight shutdown work may be lost).\n",
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
