package main

import (
	"errors"
	"testing"
)

// Tests for the unit-aware upgrade respawn (#796) and the unconditional
// fallback (#813). All three collaborators are stubbed so nothing here
// touches the real systemctl/launchctl or spawns a daemon process — a real
// supervised daemon may be running on the machine executing these tests.

// stubRespawnCollaborators replaces the autostart-detection, unit-restart,
// ad-hoc-spawn, and shutdown-wait hooks used by respawnDaemonAfterUpgrade,
// restoring them on cleanup. The shutdown wait is stubbed to an immediate nil
// so no test here pings the host's control socket — a real supervised daemon
// answering it would stall the wait for its full grace. It returns counters
// for the restart and ad-hoc paths.
func stubRespawnCollaborators(t *testing.T, installed bool, restartErr error) (restartCalls, ensureCalls *int) {
	t.Helper()
	prevInstalled := autostartInstalledFn
	prevRestart := restartAutostartUnitFn
	prevEnsure := ensureDaemonFn
	prevWait := waitForShutdownCompletionFn
	t.Cleanup(func() {
		autostartInstalledFn = prevInstalled
		restartAutostartUnitFn = prevRestart
		ensureDaemonFn = prevEnsure
		waitForShutdownCompletionFn = prevWait
	})
	restartCalls = new(int)
	ensureCalls = new(int)
	autostartInstalledFn = func() bool { return installed }
	restartAutostartUnitFn = func() error {
		*restartCalls++
		return restartErr
	}
	ensureDaemonFn = func() error {
		*ensureCalls++
		return nil
	}
	waitForShutdownCompletionFn = func() error { return nil }
	return restartCalls, ensureCalls
}

// TestRespawnAfterUpgradeRestartsInstalledUnit pins the #796 fix: when the
// autostart unit is installed, the post-upgrade respawn must go through the
// service manager so the daemon stays supervised, and must NOT spawn an
// ad-hoc child.
func TestRespawnAfterUpgradeRestartsInstalledUnit(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, nil)

	respawnDaemonAfterUpgrade()

	if *restartCalls != 1 {
		t.Fatalf("unit restarts = %d, want 1", *restartCalls)
	}
	if *ensureCalls != 0 {
		t.Fatalf("ad-hoc spawns = %d, want 0 (unit restart must not be demoted to an ad-hoc daemon)", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeWithoutUnitSpawnsAdHoc: installs without an
// autostart unit fall back to an ad-hoc daemon spawn.
func TestRespawnAfterUpgradeWithoutUnitSpawnsAdHoc(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, false, nil)

	respawnDaemonAfterUpgrade()

	if *restartCalls != 0 {
		t.Fatalf("unit restarts = %d, want 0 when no unit is installed", *restartCalls)
	}
	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeFallsBackWhenRestartFails: a failing
// systemctl/launchctl invocation must not leave task schedules dark — the
// respawn falls back to the ad-hoc spawn.
func TestRespawnAfterUpgradeFallsBackWhenRestartFails(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, errors.New("systemctl exited 1"))

	respawnDaemonAfterUpgrade()

	if *restartCalls != 1 {
		t.Fatalf("unit restarts = %d, want 1", *restartCalls)
	}
	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1 (fallback after a failed unit restart)", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeSpawnsWithZeroEnabledTasks pins the #813 fix: the
// post-upgrade fallback must respawn unconditionally, not only when an
// enabled task exists. Callers only invoke the respawn after stopping a
// running daemon, and that daemon may have been serving autoyes mode alone.
// AGENT_FACTORY_HOME points at an empty temp dir so the task store is
// guaranteed empty — if the enabled-task gate ever creeps back into the
// respawn path, this test fails.
func TestRespawnAfterUpgradeSpawnsWithZeroEnabledTasks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, ensureCalls := stubRespawnCollaborators(t, false, nil)

	respawnDaemonAfterUpgrade()

	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1 even with zero enabled tasks (autoyes-only daemon must be restored, #813)", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeWaitsForShutdownFirst pins the #854 fix: the Shutdown
// RPC acks before the old daemon tears down, so the respawn must wait for the
// control socket to die before EITHER respawn branch runs — otherwise the new
// daemon (ad-hoc EnsureDaemon ping or the unit-restarted daemon's startup ping
// guard) sees the dying daemon as alive, skips the spawn, and nothing is left
// running once it exits. Both branches are exercised; a wait timeout must
// degrade to a respawn attempt, never a skipped one.
func TestRespawnAfterUpgradeWaitsForShutdownFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		waitErr   error
		wantStep  string
	}{
		{name: "ad-hoc branch", installed: false, wantStep: "ensure"},
		{name: "unit branch", installed: true, wantStep: "restart"},
		{name: "wait timeout still respawns", installed: false, waitErr: errors.New("daemon control socket still answering"), wantStep: "ensure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRespawnCollaborators(t, tc.installed, nil)
			var seq []string
			waitForShutdownCompletionFn = func() error {
				seq = append(seq, "wait")
				return tc.waitErr
			}
			prevRestart, prevEnsure := restartAutostartUnitFn, ensureDaemonFn
			restartAutostartUnitFn = func() error {
				seq = append(seq, "restart")
				return prevRestart()
			}
			ensureDaemonFn = func() error {
				seq = append(seq, "ensure")
				return prevEnsure()
			}

			respawnDaemonAfterUpgrade()

			if len(seq) != 2 || seq[0] != "wait" || seq[1] != tc.wantStep {
				t.Fatalf("call sequence = %v, want [wait %s]", seq, tc.wantStep)
			}
		})
	}
}
