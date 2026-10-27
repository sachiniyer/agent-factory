package commands

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
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
			want: "The old daemon is still finishing its shutdown (pid 4242) — it normally exits on its own: wait until `af daemon status` no longer shows pid 4242 as verified, then run af again (running af sooner can kill it mid-shutdown). " +
				"If it still does after several minutes, it may be wedged: kill -9 4242 (in-flight shutdown work may be lost).\n",
		},
		{
			pid: 0,
			want: "The old daemon is still finishing its shutdown — it normally exits on its own: wait until `af daemon status` no longer shows a verified pid, then run af again (running af sooner can kill it mid-shutdown). " +
				"If it still does after several minutes, it may be wedged: kill -9 that pid (in-flight shutdown work may be lost).\n",
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

// TestShutdownIncompleteHintWaitsForExitBeforeAf: the old daemon closes its
// control socket before its final save, and an af run in that window finds no
// socket and reclaims the home through StopDaemon — SIGTERM, then SIGKILL — so
// the hint must never send the user to run af before the old process is gone.
// And the user acts minutes later, so the check they run must re-verify the
// daemon's identity at that moment: `af daemon status` re-checks the home's
// recorded pid is a live `af --daemon` each time it runs (and never starts a
// daemon), where a bare `ps -p` passes for whatever process reused the pid and
// "a leftover `af --daemon`" may be another home's daemon.
func TestShutdownIncompleteHintWaitsForExitBeforeAf(t *testing.T) {
	for _, pid := range []int{0, 4242} {
		hint := shutdownIncompleteHint(pid)
		runAf := strings.Index(hint, "run af again")
		waitExit := strings.Index(hint, "wait until `af daemon status`")
		if runAf < 0 || waitExit < 0 || waitExit > runAf {
			t.Fatalf("pid=%d: hint %q must tell the user to wait until `af daemon status` no longer shows the old daemon before running af", pid, hint)
		}
		for _, unsafe := range []string{"wait a moment", "ps -p", "leftover"} {
			if strings.Contains(hint, unsafe) {
				t.Fatalf("pid=%d: hint %q contains %q, which does not re-verify the daemon's identity when the user acts", pid, hint, unsafe)
			}
		}
	}
}
