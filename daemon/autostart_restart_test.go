package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// stubAutostartStopDaemon replaces the ad-hoc daemon stop InstallAutostart runs
// during the handover with one that returns the given result, restoring the
// production value on cleanup. Lets the install path be exercised without
// signaling a real daemon.
func stubAutostartStopDaemon(t *testing.T, stopped bool, err error) {
	t.Helper()
	prev := autostartStopDaemon
	t.Cleanup(func() { autostartStopDaemon = prev })
	autostartStopDaemon = func() (bool, error) { return stopped, err }
}

// stubAutostartUnitCommandFunc replaces the external command runner with fn,
// recording each invocation, restoring the production value on cleanup. Use it
// when a test needs a step (e.g. enable/load) to fail while the others succeed.
func stubAutostartUnitCommandFunc(t *testing.T, fn func(name string, args ...string) ([]byte, error)) *[][]string {
	t.Helper()
	prev := autostartUnitCommand
	t.Cleanup(func() { autostartUnitCommand = prev })
	var calls [][]string
	autostartUnitCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return fn(name, args...)
	}
	return &calls
}

// calledWith reports whether the recorded invocations include an exact match
// for the given command and arguments.
func calledWith(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if strings.Join(c, " ") == strings.Join(want, " ") {
			return true
		}
	}
	return false
}

// TestInstallAutostartEnablesUnitWhenStopDaemonFailsLinux is the core #974
// guard: a failed ad-hoc→supervised daemon handover must NOT abort the install
// before the unit is enabled. After InstallAutostart returns successfully the
// unit file must be present AND the enable command must have run, so
// AutostartInstalled does not later report a never-enabled unit as installed.
func TestInstallAutostartEnablesUnitWhenStopDaemonFailsLinux(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)
	stubAutostartStopDaemon(t, false, errors.New("could not stop ad-hoc daemon"))

	unitPath, err := InstallAutostart()
	if err != nil {
		t.Fatalf("InstallAutostart returned error despite a recoverable stop failure: %v", err)
	}
	if want := filepath.Join(dir, autostartUnitName); unitPath != want {
		t.Fatalf("unit path = %q, want %q", unitPath, want)
	}
	if _, statErr := os.Stat(unitPath); statErr != nil {
		t.Fatalf("unit file missing after install: %v", statErr)
	}
	if !AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = false after a successful install")
	}
	if !calledWith(*calls, "systemctl", "--user", "enable", "--now", autostartUnitName) {
		t.Fatalf("enable did not run after the stop failure; calls = %v", *calls)
	}
}

// TestInstallAutostartLoadsAgentWhenStopDaemonFailsDarwin is the macOS variant:
// a stop failure must not block the launchctl bootstrap that makes autostart
// real.
func TestInstallAutostartLoadsAgentWhenStopDaemonFailsDarwin(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	calls := stubAutostartUnitCommand(t, nil)
	stubAutostartStopDaemon(t, false, errors.New("could not stop ad-hoc daemon"))

	plistPath, err := InstallAutostart()
	if err != nil {
		t.Fatalf("InstallAutostart returned error despite a recoverable stop failure: %v", err)
	}
	if want := filepath.Join(dir, autostartLaunchdLabel+".plist"); plistPath != want {
		t.Fatalf("plist path = %q, want %q", plistPath, want)
	}
	if _, statErr := os.Stat(plistPath); statErr != nil {
		t.Fatalf("plist missing after install: %v", statErr)
	}
	if !AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = false after a successful install")
	}
	if !calledWith(*calls, "launchctl", "bootstrap", launchdGUIDomain(), plistPath) {
		t.Fatalf("bootstrap did not run after the stop failure; calls = %v", *calls)
	}
}

// TestInstallAutostartRemovesUnitWhenEnableFailsLinux pins the #974 cleanup:
// a hard enable failure must not leave a present-but-not-enabled unit file that
// AutostartInstalled would misreport as installed.
func TestInstallAutostartRemovesUnitWhenEnableFailsLinux(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	stubAutostartStopDaemon(t, true, nil)
	stubAutostartUnitCommandFunc(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "enable" {
			return []byte("enable failed"), errors.New("exit status 1")
		}
		return nil, nil
	})

	if _, err := InstallAutostart(); err == nil {
		t.Fatalf("InstallAutostart succeeded despite a failing enable")
	}
	if _, statErr := os.Stat(filepath.Join(dir, autostartUnitName)); !os.IsNotExist(statErr) {
		t.Fatalf("unit file should be cleaned up after enable failure; stat err = %v", statErr)
	}
	if AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = true after a failed install left no enabled unit")
	}
}

// TestInstallAutostartRemovesPlistWhenLoadFailsDarwin is the macOS cleanup
// variant: a hard launchctl bootstrap failure must not leave an orphaned plist.
func TestInstallAutostartRemovesPlistWhenLoadFailsDarwin(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	stubAutostartStopDaemon(t, true, nil)
	stubAutostartUnitCommandFunc(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "bootstrap" {
			return []byte("bootstrap failed"), errors.New("exit status 1")
		}
		return nil, nil
	})

	if _, err := InstallAutostart(); err == nil {
		t.Fatalf("InstallAutostart succeeded despite a failing bootstrap")
	}
	if _, statErr := os.Stat(filepath.Join(dir, autostartLaunchdLabel+".plist")); !os.IsNotExist(statErr) {
		t.Fatalf("plist should be cleaned up after load failure; stat err = %v", statErr)
	}
	if AutostartInstalled() {
		t.Fatalf("AutostartInstalled() = true after a failed install left no loaded agent")
	}
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

// TestRefreshAutostartUnitRewritesLegacyLinuxUnit is the upgrade half of
// #2176: changing the renderer cannot repair the unit an existing install
// already has on disk. The migration is deliberately surgical so #2168's
// StartLimit and exit-status policy can land in the same template unchanged.
func TestRefreshAutostartUnitRewritesLegacyLinuxUnit(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)
	unitPath := filepath.Join(dir, autostartUnitName)
	legacy := "[Unit]\n" +
		"Description=Agent Factory daemon\n" +
		"StartLimitIntervalSec=60\n" +
		"StartLimitBurst=5\n\n" +
		"[Service]\n" +
		"ExecStart=\"/opt/af\" --daemon\n" +
		"Restart=on-failure\n" +
		"RestartPreventExitStatus=78\n" +
		"Environment=PATH=/custom/bin\n\n" +
		"[Install]\nWantedBy=default.target\n"
	if err := os.WriteFile(unitPath, []byte(legacy), 0644); err != nil {
		t.Fatalf("seed legacy unit: %v", err)
	}

	if err := RefreshAutostartUnit(); err != nil {
		t.Fatalf("RefreshAutostartUnit: %v", err)
	}
	got, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read refreshed unit: %v", err)
	}
	text := string(got)
	if strings.Count(text, "KillMode=process\n") != 1 {
		t.Fatalf("refreshed unit must contain exactly one safe KillMode:\n%s", text)
	}
	for _, preserved := range []string{
		"StartLimitIntervalSec=60",
		"StartLimitBurst=5",
		"RestartPreventExitStatus=78",
		"ExecStart=\"/opt/af\" --daemon",
		"Environment=PATH=/custom/bin",
	} {
		if !strings.Contains(text, preserved) {
			t.Errorf("migration dropped future/existing directive %q:\n%s", preserved, text)
		}
	}
	if !calledWith(*calls, "systemctl", "--user", "daemon-reload") {
		t.Fatalf("rewritten unit was not reloaded; calls=%v", *calls)
	}
}

func TestRefreshAutostartUnitReplacesUnsafeAndDuplicateKillModes(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	stubAutostartUnitCommand(t, nil)
	unitPath := filepath.Join(dir, autostartUnitName)
	legacy := "[Unit]\nDescription=x\n\n[Service]\nKillMode=mixed\nExecStart=/opt/af --daemon\nKillMode=control-group\n"
	if err := os.WriteFile(unitPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAutostartUnit(); err != nil {
		t.Fatalf("RefreshAutostartUnit: %v", err)
	}
	got, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "KillMode=") != 1 || !strings.Contains(string(got), "KillMode=process\n") {
		t.Fatalf("unsafe/duplicate KillMode directives survived:\n%s", got)
	}
}

func TestRefreshAutostartUnitReloadsAlreadySafeUnit(t *testing.T) {
	dir := withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)
	unitPath := filepath.Join(dir, autostartUnitName)
	unit := "[Service]\nKillMode=process\nExecStart=/opt/af --daemon\n"
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RefreshAutostartUnit(); err != nil {
		t.Fatalf("RefreshAutostartUnit: %v", err)
	}
	got, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != unit {
		t.Fatalf("already-safe unit changed:\n got=%q\nwant=%q", got, unit)
	}
	if !calledWith(*calls, "systemctl", "--user", "daemon-reload") {
		t.Fatalf("already-safe unit was not reloaded after a possible prior reload failure; calls=%v", *calls)
	}
}

func TestRefreshAutostartUnitDarwinLeavesPlistUntouched(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	calls := stubAutostartUnitCommand(t, nil)
	plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
	plist := "<plist><dict><key>Label</key><string>com.agent-factory.daemon</string></dict></plist>\n"
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RefreshAutostartUnit(); err != nil {
		t.Fatalf("RefreshAutostartUnit(darwin): %v", err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plist {
		t.Fatalf("Linux cgroup migration rewrote the launchd plist:\n got=%q\nwant=%q", got, plist)
	}
	if len(*calls) != 0 {
		t.Fatalf("Darwin refresh invoked a service-manager command: %v", *calls)
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

// TestAutostartLaunchdCallsShareOneDomain is the #1947 mechanism-3 repro:
// every launchctl verb af issues must name the SAME launchd domain.
//
// InstallAutostart used the legacy `launchctl load`, which takes no domain and
// bootstraps into the CALLING session's — user/<uid> over ssh, gui/<uid> from a
// Terminal window — while RestartAutostartUnit has always kicked gui/<uid>
// explicitly. When they disagree, the kickstart cannot find the job: the
// post-upgrade restart silently no-ops, the stale daemon keeps serving the old
// binary, and `af upgrade` prints success anyway. That is the macOS half of the
// stale-daemon report, and #1920 independently found the same case.
//
// Asserting exact argv (rather than "contains gui/") is the point: a legacy
// verb passes any domain-substring check by simply having no domain at all.
func TestAutostartLaunchdCallsShareOneDomain(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	dir := withAutostartTestEnv(t, "darwin")
	stubAutostartStopDaemon(t, true, nil)
	calls := stubAutostartUnitCommand(t, nil)
	plist := filepath.Join(dir, autostartLaunchdLabel+".plist")

	if _, err := InstallAutostart(); err != nil {
		t.Fatalf("InstallAutostart: %v", err)
	}
	if err := RestartAutostartUnit(); err != nil {
		t.Fatalf("RestartAutostartUnit: %v", err)
	}
	if err := PauseAutostartUnit(); err != nil {
		t.Fatalf("PauseAutostartUnit: %v", err)
	}
	if err := ResumeAutostartUnit(); err != nil {
		t.Fatalf("ResumeAutostartUnit: %v", err)
	}
	if _, err := UninstallAutostart(); err != nil {
		t.Fatalf("UninstallAutostart: %v", err)
	}

	// Computed here rather than via the production helpers, so a change to
	// those cannot silently redefine what this test is checking.
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + autostartLaunchdLabel
	want := [][]string{
		{"launchctl", "bootout", target},          // install: displace any previous agent
		{"launchctl", "bootstrap", domain, plist}, // install
		{"launchctl", "kickstart", "-k", target},  // restart
		{"launchctl", "bootout", target},          // pause
		{"launchctl", "bootstrap", domain, plist}, // resume
		{"launchctl", "bootout", target},          // uninstall
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("launchctl calls must all target %s.\n got=%v\nwant=%v", domain, *calls, want)
	}
	for _, c := range *calls {
		if len(c) > 1 && (c[1] == "load" || c[1] == "unload") {
			t.Fatalf("launchctl %s takes no domain and lands in the caller's session domain, which kickstart gui/<uid> may never see: %v", c[1], c)
		}
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

// TestInstallUninstallRoundTripHonorsXDGConfigHomeLinux drives install and
// uninstall through the REAL directory resolver (not a stubbed one) with
// XDG_CONFIG_HOME pointing at a temp dir, pinning the #1091 contract: both
// operations target $XDG_CONFIG_HOME/systemd/user — the directory systemd
// actually scans — never ~/.config. Commands and the daemon stop are stubbed,
// so no real unit is enabled and no real daemon is signaled.
func TestInstallUninstallRoundTripHonorsXDGConfigHomeLinux(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	prevGOOS := autostartGOOS
	t.Cleanup(func() { autostartGOOS = prevGOOS })
	autostartGOOS = "linux"
	calls := stubAutostartUnitCommand(t, nil)
	stubAutostartStopDaemon(t, true, nil)

	wantPath := filepath.Join(xdg, "systemd", "user", autostartUnitName)

	unitPath, err := InstallAutostart()
	if err != nil {
		t.Fatalf("InstallAutostart: %v", err)
	}
	if unitPath != wantPath {
		t.Fatalf("install wrote %q, want XDG-resolved %q", unitPath, wantPath)
	}
	if _, statErr := os.Stat(unitPath); statErr != nil {
		t.Fatalf("unit file missing after install: %v", statErr)
	}

	removedPath, err := UninstallAutostart()
	if err != nil {
		t.Fatalf("UninstallAutostart: %v", err)
	}
	if removedPath != wantPath {
		t.Fatalf("uninstall targeted %q, want the same XDG-resolved %q install wrote", removedPath, wantPath)
	}
	if _, statErr := os.Stat(wantPath); !os.IsNotExist(statErr) {
		t.Fatalf("unit file should be gone after uninstall; stat err = %v", statErr)
	}
	if !calledWith(*calls, "systemctl", "--user", "disable", "--now", autostartUnitName) {
		t.Fatalf("uninstall did not disable the unit; calls = %v", *calls)
	}
}

// TestUninstallAutostartNoUnitInstalledLinux: uninstall with nothing installed
// is a silent no-op that runs no service-manager commands.
func TestUninstallAutostartNoUnitInstalledLinux(t *testing.T) {
	withAutostartTestEnv(t, "linux")
	calls := stubAutostartUnitCommand(t, nil)

	path, err := UninstallAutostart()
	if err != nil {
		t.Fatalf("UninstallAutostart: %v", err)
	}
	if path != "" {
		t.Fatalf("removed path = %q, want empty for a no-op uninstall", path)
	}
	if len(*calls) != 0 {
		t.Fatalf("no commands should run when no unit is installed, got %v", *calls)
	}
}

// TestUninstallAutostartDarwin verifies the launchd uninstall resolves the
// plist through the injected LaunchAgents resolver and unloads before removing.
func TestUninstallAutostartDarwin(t *testing.T) {
	dir := withAutostartTestEnv(t, "darwin")
	calls := stubAutostartUnitCommand(t, nil)
	plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>\n"), 0644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	removedPath, err := UninstallAutostart()
	if err != nil {
		t.Fatalf("UninstallAutostart: %v", err)
	}
	if removedPath != plistPath {
		t.Fatalf("removed path = %q, want %q", removedPath, plistPath)
	}
	if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
		t.Fatalf("plist should be gone after uninstall; stat err = %v", statErr)
	}
	if !calledWith(*calls, "launchctl", "bootout", launchdServiceTarget()) {
		t.Fatalf("uninstall did not boot the agent out; calls = %v", *calls)
	}
}
