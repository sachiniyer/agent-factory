package daemon

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon autostart unit is the single OS-level unit agent-factory
// installs: a user-level systemd service on Linux or a launchd agent on
// macOS that keeps the daemon (and therefore its task scheduler and watcher
// supervisor) running across logins and reboots. Per-task timer units no
// longer exist (#782).

const (
	autostartUnitName     = "agent-factory-daemon.service"
	autostartLaunchdLabel = "com.agent-factory.daemon"
)

// quoteExecStartPath quotes a path for use in an ExecStart= line.
// systemd parses ExecStart= with shell-like quoting, so surrounding the path
// in double quotes allows spaces. Internal backslashes and double quotes are
// escaped so the value remains syntactically valid.
func quoteExecStartPath(p string) string {
	escaped := strings.ReplaceAll(p, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, `$`, `$$`)
	escaped = strings.ReplaceAll(escaped, `%`, `%%`)
	return `"` + escaped + `"`
}

// formatSystemdEnvLine renders one `Environment=NAME=value` line so systemd
// parses the whole value, spaces and all.
//
// systemd tokenizes Environment= with quote-aware, C-unescaping parsing
// (systemd.exec(5)): unquoted whitespace splits a single line into several
// independent assignments, and an unquoted backslash or quote is consumed as
// an escape. So when the value contains whitespace, a quote, or a backslash,
// the entire NAME=value is wrapped in double quotes with backslashes and
// double quotes escaped C-style inside. Values with no such characters are
// emitted bare to avoid needless churn.
//
// '%' is a systemd specifier and is always doubled. '$' is left literal:
// unlike ExecStart=, Environment= performs no variable expansion (verified
// with `systemctl show` / `systemd-analyze verify`), so doubling it would
// corrupt the value into a literal "$$". Newlines and carriage returns, which
// systemd rejects in unit files, are folded to spaces.
func formatSystemdEnvLine(name, value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, `%`, `%%`)
	assignment := name + "=" + value
	if !strings.ContainsAny(value, " \t\"'\\") {
		return "Environment=" + assignment
	}
	assignment = strings.ReplaceAll(assignment, `\`, `\\`)
	assignment = strings.ReplaceAll(assignment, `"`, `\"`)
	return `Environment="` + assignment + `"`
}

// systemdAutostartUnit renders the user-level systemd service that keeps the
// daemon running. PATH and SHELL are captured at install time because the
// systemd user manager starts services with a minimal environment, and the
// daemon needs the user's real PATH to find tmux and the agent programs.
// AGENT_FACTORY_HOME is captured for the same reason when the installing
// shell has it set: without it the unit's daemon would serve the default
// home instead of the custom one (#782).
func systemdAutostartUnit(execPath, pathEnv, shellEnv, agentFactoryHome string) string {
	envLines := formatSystemdEnvLine("PATH", pathEnv) + "\n" + formatSystemdEnvLine("SHELL", shellEnv)
	if agentFactoryHome != "" {
		envLines += "\n" + formatSystemdEnvLine("AGENT_FACTORY_HOME", agentFactoryHome)
	}
	return fmt.Sprintf(`[Unit]
Description=Agent Factory daemon (task scheduler + autoyes)

[Service]
ExecStart=%s --daemon
Restart=on-failure
RestartSec=5
%s

[Install]
WantedBy=default.target
`, quoteExecStartPath(execPath), envLines)
}

// launchdAutostartPlist renders the launchd agent that keeps the daemon
// running on macOS. KeepAlive on unsuccessful exit mirrors the systemd
// Restart=on-failure behavior; logPath captures crashes that happen before
// the daemon's own logging is up. AGENT_FACTORY_HOME is captured when set,
// matching the systemd unit (#782).
//
// Unlike the systemd Environment= line (#893), no extra quoting is needed for
// values with spaces: each value is its own XML <string> element, so spaces
// are ordinary text content. html.EscapeString only needs to neutralize the
// XML metacharacters (& < > " ') that would otherwise break the markup.
func launchdAutostartPlist(execPath, pathEnv, shellEnv, agentFactoryHome, logPath string) string {
	esc := html.EscapeString
	homeEntry := ""
	if agentFactoryHome != "" {
		homeEntry = fmt.Sprintf("        <key>AGENT_FACTORY_HOME</key>\n        <string>%s</string>\n", esc(agentFactoryHome))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>--daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>SHELL</key>
        <string>%s</string>
%s    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, autostartLaunchdLabel, esc(execPath), esc(pathEnv), esc(shellEnv), homeEntry, esc(logPath), esc(logPath))
}

// InstallAutostart registers the daemon for autostart at login: a systemd
// user service on Linux, a launchd agent on macOS. Any daemon started ad hoc
// (by EnsureDaemon) is handed over to the supervised unit. Returns the path of
// the installed unit file.
//
// The invariant this function guarantees (#974): on success the unit is
// ENABLED/LOADED, never merely written. Enabling (and `--now` starting) the
// unit is the thing that makes autostart real and supervises future runs, so
// it must not be skipped. Two failure modes used to break that:
//
//   - The ad-hoc→supervised handover stop is best-effort. A StopDaemon failure
//     no longer aborts before enabling: the unit's `--now` start takes over the
//     control socket per the #796/#798 handover, and the secondary ad-hoc
//     daemon is left at worst enabled-but-inactive until the next login — far
//     better than a present-but-not-enabled unit. (Previously a stop failure
//     returned early, leaving the file on disk but never enabled.)
//   - On a hard failure of the reload/enable/load step the just-written unit
//     file is removed, so AutostartInstalled — which reports on file existence —
//     can never misreport a not-enabled unit as installed and mislead the
//     upgrade respawn path into RestartAutostartUnit on a unit that was never
//     enabled.
func InstallAutostart() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}

	switch autostartGOOS {
	case "linux":
		dir, err := autostartSystemdUserDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve systemd user directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create systemd user directory: %w", err)
		}
		unitPath := filepath.Join(dir, autostartUnitName)
		content := systemdAutostartUnit(execPath, os.Getenv("PATH"), os.Getenv("SHELL"), os.Getenv("AGENT_FACTORY_HOME"))
		if err := config.AtomicWriteFile(unitPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write unit file: %w", err)
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "daemon-reload"); err != nil {
			removeAutostartUnitFile(unitPath)
			return "", fmt.Errorf("failed to reload systemd user daemon: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		// Hand any ad-hoc daemon over to the supervised one, but never let a
		// stop failure block enabling — see the function comment (#974).
		if _, err := autostartStopDaemon(); err != nil {
			log.WarningLog.Printf("failed to stop the running daemon before enabling the autostart unit; enabling anyway: %v", err)
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "enable", "--now", autostartUnitName); err != nil {
			removeAutostartUnitFile(unitPath)
			return "", fmt.Errorf("failed to enable daemon service: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return unitPath, nil

	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve LaunchAgents directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
		configDir, err := config.GetConfigDir()
		if err != nil {
			return "", fmt.Errorf("failed to get config directory: %w", err)
		}
		logPath := filepath.Join(configDir, "daemon-launchd.log")
		content := launchdAutostartPlist(execPath, os.Getenv("PATH"), os.Getenv("SHELL"), os.Getenv("AGENT_FACTORY_HOME"), logPath)
		// Unload a previous agent first so launchctl load picks up the new file.
		_, _ = autostartUnitCommand("launchctl", "unload", plistPath)
		if err := config.AtomicWriteFile(plistPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write plist file: %w", err)
		}
		// Hand any ad-hoc daemon over to the supervised one, but never let a
		// stop failure block loading — see the function comment (#974).
		if _, err := autostartStopDaemon(); err != nil {
			log.WarningLog.Printf("failed to stop the running daemon before loading the autostart agent; loading anyway: %v", err)
		}
		if out, err := autostartUnitCommand("launchctl", "load", plistPath); err != nil {
			removeAutostartUnitFile(plistPath)
			return "", fmt.Errorf("failed to load launch agent: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return plistPath, nil

	default:
		return "", fmt.Errorf("daemon autostart is not supported on %s (the daemon still starts automatically whenever you run af)", autostartGOOS)
	}
}

// removeAutostartUnitFile deletes a just-written autostart unit/plist after a
// later install step fails, so a hard failure never leaves a present-but-not-
// enabled file that AutostartInstalled would misreport as installed (#974).
// Best-effort: a cleanup failure is logged, not surfaced, since the original
// install error is the one the caller must act on.
func removeAutostartUnitFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to clean up autostart unit %s after a failed install: %v", path, err)
	}
}

// Injection points for tests, mirroring legacy_units.go: the GOOS the install
// and restart helpers act on, the unit-directory resolvers, the external
// command runner, and the ad-hoc daemon stop, so the install and upgrade
// respawn paths can be exercised hermetically without touching the host's real
// autostart unit, invoking systemctl/launchctl, or signaling a real daemon.
var (
	autostartGOOS            = runtime.GOOS
	autostartSystemdUserDir  = defaultSystemdUserDir
	autostartLaunchAgentsDir = defaultLaunchAgentsDir
	autostartUnitCommand     = runAutostartUnitCommand
	autostartStopDaemon      = StopDaemon
)

func runAutostartUnitCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// autostartUnitFilePath returns the path of the unit file InstallAutostart
// writes for the current platform, or "" when autostart is unsupported here.
func autostartUnitFilePath() (string, error) {
	switch autostartGOOS {
	case "linux":
		dir, err := autostartSystemdUserDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, autostartUnitName), nil
	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, autostartLaunchdLabel+".plist"), nil
	default:
		return "", nil
	}
}

// AutostartInstalled reports whether the daemon autostart unit installed by
// InstallAutostart is present on this machine.
func AutostartInstalled() bool {
	path, err := autostartUnitFilePath()
	if err != nil || path == "" {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// RestartAutostartUnit restarts the daemon through the OS service manager —
// `systemctl --user restart` on Linux, `launchctl kickstart -k` on macOS — so
// the daemon stays supervised. Used by the upgrade/auto-update respawn path
// (#796): the Shutdown RPC is a clean exit that Restart=on-failure
// deliberately does not restart, so respawning via launchDaemonProcess would
// leave the unit inactive and demote the daemon to an unsupervised ad-hoc
// child until the next login.
func RestartAutostartUnit() error {
	switch autostartGOOS {
	case "linux":
		if out, err := autostartUnitCommand("systemctl", "--user", "restart", autostartUnitName); err != nil {
			return fmt.Errorf("systemctl --user restart %s failed: %w\n%s", autostartUnitName, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", os.Getuid(), autostartLaunchdLabel)
		if out, err := autostartUnitCommand("launchctl", "kickstart", "-k", target); err != nil {
			return fmt.Errorf("launchctl kickstart -k %s failed: %w\n%s", target, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

// UninstallAutostart removes the daemon autostart unit installed by
// InstallAutostart. The daemon itself keeps running until it exits or is
// stopped; it just no longer restarts at login. Returns the path of the
// removed unit file, or "" if none was installed.
func UninstallAutostart() (string, error) {
	switch runtime.GOOS {
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		unitPath := filepath.Join(home, ".config", "systemd", "user", autostartUnitName)
		if _, err := os.Stat(unitPath); os.IsNotExist(err) {
			return "", nil
		}
		if out, err := exec.Command("systemctl", "--user", "disable", "--now", autostartUnitName).CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to disable daemon service: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove unit file: %w", err)
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to reload systemd user daemon: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return unitPath, nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		plistPath := filepath.Join(home, "Library", "LaunchAgents", autostartLaunchdLabel+".plist")
		if _, err := os.Stat(plistPath); os.IsNotExist(err) {
			return "", nil
		}
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove plist file: %w", err)
		}
		return plistPath, nil

	default:
		return "", fmt.Errorf("daemon autostart is not supported on %s", runtime.GOOS)
	}
}
