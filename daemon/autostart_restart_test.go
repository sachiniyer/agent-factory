package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the unit-aware daemon restart used by the upgrade/auto-update
// respawn path (#796). Everything is exercised against temp directories and
// an injected command runner — a real supervised daemon may be running on the
// machine executing these tests, and they must never touch its actual
// systemctl/launchctl unit.

// withAutostartTestEnv points the autostart helpers at the given GOOS and a
// temp unit directory, restoring the production values on cleanup. It returns
// the unit directory.
func withAutostartTestEnv(t *testing.T, goos string) string {
	t.Helper()
	dir := t.TempDir()
	prevGOOS := autostartGOOS
	prevSystemd := autostartSystemdUserDir
	prevLaunchd := autostartLaunchAgentsDir
	t.Cleanup(func() {
		autostartGOOS = prevGOOS
		autostartSystemdUserDir = prevSystemd
		autostartLaunchAgentsDir = prevLaunchd
	})
	autostartGOOS = goos
	autostartSystemdUserDir = func() (string, error) { return dir, nil }
	autostartLaunchAgentsDir = func() (string, error) { return dir, nil }
	return dir
}

// stubAutostartUnitCommand replaces the external command runner with one that
// records each invocation, restoring it on cleanup. Each recorded call is the
// command name followed by its arguments. The runner returns runErr (and, when
// failing, some captured output so callers can assert it surfaces in errors).
func stubAutostartUnitCommand(t *testing.T, runErr error) *[][]string {
	t.Helper()
	prev := autostartUnitCommand
	t.Cleanup(func() { autostartUnitCommand = prev })
	var calls [][]string
	autostartUnitCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if runErr != nil {
			return []byte("unit failure detail"), runErr
		}
		return nil, nil
	}
	return &calls
}

func TestAutostartInstalledLinux(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")

	if AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = true with no unit file in %s", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, autostartUnitName), []byte("[Unit]\n"), 0644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	if !AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = false with unit file present in %s", dir)
	}
}

func TestAutostartInstalledDarwin(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")

	if AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = true with no plist in %s", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, autostartLaunchdLabel+".plist"), []byte("<plist/>\n"), 0644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}
	if !AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = false with plist present in %s", dir)
	}
}

func TestAutostartInstalledUnsupportedGOOS(t *testing.T) {
	withAutostartTestEnv(t, "windows")
	if AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = true on unsupported GOOS")
	}
}

func TestRestartAutostartUnitLinux(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)

	if err := RestartAutostartUnit(); err != nil {
		t.Fatalf("RestartAutostartUnit: %v", err)
	}
	want := [][]string{{"systemctl", "--user", "restart", autostartUnitName}}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != strings.Join(want[0], " ") {
		t.Fatalf("unit commands = %v, want %v", *calls, want)
	}
}

func TestRestartAutostartUnitDarwin(t *testing.T) {
	withAutostartTestEnv(t, "darwin")
	calls := stubAutostartUnitCommand(t, nil)

	if err := RestartAutostartUnit(); err != nil {
		t.Fatalf("RestartAutostartUnit: %v", err)
	}
	wantTarget := fmt.Sprintf("gui/%d/%s", os.Getuid(), autostartLaunchdLabel)
	want := []string{"launchctl", "kickstart", "-k", wantTarget}
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != strings.Join(want, " ") {
		t.Fatalf("unit commands = %v, want [%v]", *calls, want)
	}
}

func TestRestartAutostartUnitFailureSurfacesOutput(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	stubAutostartUnitCommand(t, errors.New("exit status 1"))

	err := RestartAutostartUnit()
	if err == nil {
		t.Fatalf("RestartAutostartUnit succeeded with a failing service manager")
	}
	if !strings.Contains(err.Error(), "unit failure detail") {
		t.Fatalf("error %q does not include the command output", err)
	}
}

func TestRestartAutostartUnitUnsupportedGOOS(t *testing.T) {
	withAutostartTestEnv(t, "windows")
	calls := stubAutostartUnitCommand(t, nil)

	if err := RestartAutostartUnit(); err == nil {
		t.Fatalf("RestartAutostartUnit should fail on unsupported GOOS")
	}
	if len(*calls) != 0 {
		t.Fatalf("no service manager command should run on unsupported GOOS, got %v", *calls)
	}
}
