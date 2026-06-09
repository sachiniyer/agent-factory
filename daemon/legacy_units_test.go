package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacySweepStub captures the directories and external commands the upgrade
// sweep would touch, keeping tests hermetic.
type legacySweepStub struct {
	systemdDir      string
	launchAgentsDir string
	commands        []string
}

// stubLegacyUnitSweep points the legacy-unit sweep at fresh temp directories
// and replaces the external command runner with a recorder. Every test that
// drives RunDaemon (directly or indirectly) must call this so the sweep can
// never touch the host's real unit directories.
func stubLegacyUnitSweep(t *testing.T) *legacySweepStub {
	t.Helper()
	stub := &legacySweepStub{
		systemdDir:      t.TempDir(),
		launchAgentsDir: t.TempDir(),
	}
	origSystemd := legacySystemdUserDir
	origLaunchd := legacyLaunchAgentsDir
	origCommand := legacyUnitCommand
	legacySystemdUserDir = func() (string, error) { return stub.systemdDir, nil }
	legacyLaunchAgentsDir = func() (string, error) { return stub.launchAgentsDir, nil }
	legacyUnitCommand = func(name string, args ...string) error {
		stub.commands = append(stub.commands, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() {
		legacySystemdUserDir = origSystemd
		legacyLaunchAgentsDir = origLaunchd
		legacyUnitCommand = origCommand
	})
	return stub
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSweepLegacyTaskUnits_RemovesOldPerTaskUnits seeds unit files in the
// shapes pre-#782 versions installed (systemd timer+service pairs on Linux,
// launchd plists on macOS) and verifies the upgrade sweep disables and
// removes all of them while leaving unrelated units untouched.
func TestSweepLegacyTaskUnits_RemovesOldPerTaskUnits(t *testing.T) {
	stub := stubLegacyUnitSweep(t)

	timer := filepath.Join(stub.systemdDir, "agent-factory-task-abc12345.timer")
	service := filepath.Join(stub.systemdDir, "agent-factory-task-abc12345.service")
	// An orphaned service without its timer (partial old uninstall) must
	// also be swept.
	orphanService := filepath.Join(stub.systemdDir, "agent-factory-task-ffff0000.service")
	// Pre-rename nightly builds used the "agent-factory-sched-" prefix.
	oldPrefixTimer := filepath.Join(stub.systemdDir, "agent-factory-sched-12345678.timer")
	oldPrefixService := filepath.Join(stub.systemdDir, "agent-factory-sched-12345678.service")
	unrelatedUnit := filepath.Join(stub.systemdDir, "syncthing.service")
	// The daemon's own autostart unit must never be swept.
	daemonUnit := filepath.Join(stub.systemdDir, "agent-factory-daemon.service")
	plist := filepath.Join(stub.launchAgentsDir, "com.agent-factory.task-abc12345.plist")
	unrelatedPlist := filepath.Join(stub.launchAgentsDir, "com.example.other.plist")
	for _, p := range []string{timer, service, orphanService, oldPrefixTimer, oldPrefixService, unrelatedUnit, daemonUnit, plist, unrelatedPlist} {
		writeTestFile(t, p)
	}

	sweepLegacyTaskUnits()

	for _, p := range []string{timer, service, orphanService, oldPrefixTimer, oldPrefixService, plist} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected legacy unit %s to be removed, stat err=%v", p, err)
		}
	}
	for _, p := range []string{unrelatedUnit, daemonUnit, unrelatedPlist} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected non-task file %s to survive the sweep, stat err=%v", p, err)
		}
	}

	joined := strings.Join(stub.commands, "\n")
	if !strings.Contains(joined, "systemctl --user disable --now agent-factory-task-abc12345.timer") {
		t.Errorf("expected the legacy timer to be disabled, commands:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl --user disable --now agent-factory-sched-12345678.timer") {
		t.Errorf("expected the pre-rename legacy timer to be disabled, commands:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl --user daemon-reload") {
		t.Errorf("expected a systemd daemon-reload after removals, commands:\n%s", joined)
	}
	if !strings.Contains(joined, "launchctl unload "+plist) {
		t.Errorf("expected the legacy launch agent to be unloaded, commands:\n%s", joined)
	}
}

// TestSweepLegacyTaskUnits_NoopWhenNothingInstalled verifies a clean host
// runs no external commands at all — fresh installs must not shell out to
// systemctl/launchctl on every daemon start.
func TestSweepLegacyTaskUnits_NoopWhenNothingInstalled(t *testing.T) {
	stub := stubLegacyUnitSweep(t)
	writeTestFile(t, filepath.Join(stub.systemdDir, "syncthing.service"))

	sweepLegacyTaskUnits()

	if len(stub.commands) != 0 {
		t.Errorf("expected no external commands on a clean host, got: %v", stub.commands)
	}
}
