package daemon

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests for the pause/resume pair `af reset` wraps around its wipe so the
// service manager cannot relaunch the daemon mid-wipe. Exercised against the
// injected GOOS/unit-dir/command-runner from autostart_restart_test.go — they
// must never touch the host's real systemctl/launchctl unit.

func TestPauseAndResumeAutostartUnitLinux(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)

	if err := PauseAutostartUnit(); err != nil {
		t.Fatalf("PauseAutostartUnit: %v", err)
	}
	if err := ResumeAutostartUnit(); err != nil {
		t.Fatalf("ResumeAutostartUnit: %v", err)
	}

	want := [][]string{
		{"systemctl", "--user", "stop", autostartUnitName},
		{"systemctl", "--user", "reset-failed", autostartUnitName},
		{"systemctl", "--user", "start", autostartUnitName},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("unit commands = %v, want %v", *calls, want)
	}
}

func TestPauseAndResumeAutostartUnitDarwin(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	calls := stubAutostartUnitCommand(t, nil)

	if err := PauseAutostartUnit(); err != nil {
		t.Fatalf("PauseAutostartUnit: %v", err)
	}
	if err := ResumeAutostartUnit(); err != nil {
		t.Fatalf("ResumeAutostartUnit: %v", err)
	}

	// Pause boots the job OUT of the same gui/<uid> domain the install
	// bootstrapped it into, and resume bootstraps it back there — so a paused
	// unit is one RestartAutostartUnit's kickstart can still find (#1947).
	plist := filepath.Join(dir, autostartLaunchdLabel+".plist")
	want := [][]string{
		{"launchctl", "bootout", launchdServiceTarget()},
		{"launchctl", "bootstrap", launchdGUIDomain(), plist},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("unit commands = %v, want %v", *calls, want)
	}
}

// A failing service-manager call must surface its captured output in the
// returned error so the reset warning tells the user what actually failed.
func TestPauseAndResumeAutostartUnitSurfaceCommandFailure(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		withAutostartTestEnv(t, goos)
		stubAutostartUnitCommand(t, errors.New("boom"))

		for name, fn := range map[string]func() error{
			"PauseAutostartUnit":  PauseAutostartUnit,
			"ResumeAutostartUnit": ResumeAutostartUnit,
		} {
			err := fn()
			if err == nil {
				t.Fatalf("%s on %s: expected error, got nil", name, goos)
			}
			if !strings.Contains(err.Error(), "unit failure detail") {
				t.Errorf("%s on %s: error %q does not surface the command output", name, goos, err)
			}
		}
	}
}

func TestPauseAndResumeAutostartUnitUnsupportedGOOS(t *testing.T) {
	withAutostartTestEnv(t, "windows")
	calls := stubAutostartUnitCommand(t, nil)

	if err := PauseAutostartUnit(); err == nil {
		t.Error("PauseAutostartUnit: expected unsupported-platform error, got nil")
	}
	if err := ResumeAutostartUnit(); err == nil {
		t.Error("ResumeAutostartUnit: expected unsupported-platform error, got nil")
	}
	if len(*calls) != 0 {
		t.Errorf("no unit commands expected on an unsupported platform, got %v", *calls)
	}
}
