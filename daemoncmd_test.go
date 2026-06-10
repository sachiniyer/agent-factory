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
// and ad-hoc-spawn hooks used by respawnDaemonAfterUpgrade, restoring them on
// cleanup. It returns counters for the restart and ad-hoc paths.
func stubRespawnCollaborators(t *testing.T, installed bool, restartErr error) (restartCalls, ensureCalls *int) {
	t.Helper()
	prevInstalled := autostartInstalledFn
	prevRestart := restartAutostartUnitFn
	prevEnsure := ensureDaemonFn
	t.Cleanup(func() {
		autostartInstalledFn = prevInstalled
		restartAutostartUnitFn = prevRestart
		ensureDaemonFn = prevEnsure
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
