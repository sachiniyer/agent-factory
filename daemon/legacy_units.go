package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// Previous versions of agent-factory scheduled each task with its own OS-level
// timer unit (systemd user timer on Linux, launchd agent on macOS). Since #782
// the daemon evaluates cron expressions in-process, so those per-task units
// are obsolete and would double-fire tasks if left installed. sweepLegacyTaskUnits
// runs once at daemon start and removes them — a hard rollforward, matching
// the repo precedent of #658 (no compat shim).

// Injection points for tests: directory resolvers and the external command
// runner, so the sweep can be exercised against temp directories without
// touching the host's real unit directories or invoking systemctl/launchctl.
var (
	legacySystemdUserDir  = defaultSystemdUserDir
	legacyLaunchAgentsDir = defaultLaunchAgentsDir
	legacyUnitCommand     = runLegacyUnitCommand
)

// defaultSystemdUserDir resolves the directory the systemd user manager scans
// for user units, matching systemd's own rule (#1091): $XDG_CONFIG_HOME/systemd/user
// when XDG_CONFIG_HOME is set to an absolute path, else ~/.config/systemd/user.
// systemd ignores a relative XDG_CONFIG_HOME (per the XDG base-dir spec), so a
// relative value falls through to the home default; diverging here would make
// af write units to a directory systemd never reads.
func defaultSystemdUserDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func defaultLaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func runLegacyUnitCommand(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// sweepLegacyTaskUnits disables and deletes per-task scheduler units installed
// by pre-#782 versions. Each removal is logged once; all steps are best-effort
// so a partially-broken old install can never prevent the daemon from starting.
func sweepLegacyTaskUnits() {
	sweepLegacySystemdUnits()
	sweepLegacyLaunchdUnits()
}

// legacySystemdUnitPrefixes are the unit-name prefixes previous versions used
// for per-task timers: "agent-factory-task-" for every v1.0.x release,
// "agent-factory-sched-" for the builds between the repo rename and the
// Schedules→Tasks rename (6ec0996), and "claude-squad-sched-" for builds
// before the claude-squad→agent-factory repo rename (8e55df0). These are the
// only three prefixes any released scheduler ever wrote; launchd support
// postdates the repo rename, so "com.agent-factory.task-" below is complete.
var legacySystemdUnitPrefixes = []string{"agent-factory-task-", "agent-factory-sched-", "claude-squad-sched-"}

func sweepLegacySystemdUnits() {
	dir, err := legacySystemdUserDir()
	if err != nil {
		return
	}
	var timers, services []string
	for _, prefix := range legacySystemdUnitPrefixes {
		t, _ := filepath.Glob(filepath.Join(dir, prefix+"*.timer"))
		s, _ := filepath.Glob(filepath.Join(dir, prefix+"*.service"))
		timers = append(timers, t...)
		services = append(services, s...)
	}
	if len(timers) == 0 && len(services) == 0 {
		return
	}

	for _, timer := range timers {
		unitName := filepath.Base(timer)
		if err := legacyUnitCommand("systemctl", "--user", "disable", "--now", unitName); err != nil {
			log.WarningLog.Printf("upgrade sweep: failed to disable legacy timer %s (continuing): %v", unitName, err)
		}
	}
	for _, path := range append(timers, services...) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("upgrade sweep: failed to remove legacy unit file %s: %v", path, err)
			continue
		}
		log.InfoLog.Printf("upgrade sweep: removed legacy per-task scheduler unit %s (tasks are now scheduled by the daemon)", path)
	}
	if err := legacyUnitCommand("systemctl", "--user", "daemon-reload"); err != nil {
		log.WarningLog.Printf("upgrade sweep: systemctl --user daemon-reload failed: %v", err)
	}
}

func sweepLegacyLaunchdUnits() {
	dir, err := legacyLaunchAgentsDir()
	if err != nil {
		return
	}
	plists, _ := filepath.Glob(filepath.Join(dir, "com.agent-factory.task-*.plist"))
	for _, plist := range plists {
		if err := legacyUnitCommand("launchctl", "unload", plist); err != nil {
			label := strings.TrimSuffix(filepath.Base(plist), ".plist")
			log.WarningLog.Printf("upgrade sweep: failed to unload legacy launch agent %s (continuing): %v", label, err)
		}
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("upgrade sweep: failed to remove legacy plist %s: %v", plist, err)
			continue
		}
		log.InfoLog.Printf("upgrade sweep: removed legacy per-task scheduler unit %s (tasks are now scheduled by the daemon)", plist)
	}
}
