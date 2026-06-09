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
)

// The daemon autostart unit is the single OS-level unit agent-factory
// installs: a user-level systemd service on Linux or a launchd agent on
// macOS that keeps the daemon (and therefore the in-process task scheduler)
// running across logins and reboots. Per-task timer units no longer exist
// (#782).

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

// sanitizeEnvValue makes a value safe for an Environment= assignment.
// systemd does not apply shell-style quote parsing to Environment= values,
// so surrounding quotes would be preserved literally. Newlines are also
// disallowed by systemd in Environment= values; replace them with spaces
// rather than emitting a syntactically invalid unit file.
func sanitizeEnvValue(v string) string {
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, `$`, `$$`)
	v = strings.ReplaceAll(v, `%`, `%%`)
	return v
}

// systemdAutostartUnit renders the user-level systemd service that keeps the
// daemon running. PATH and SHELL are captured at install time because the
// systemd user manager starts services with a minimal environment, and the
// daemon needs the user's real PATH to find tmux and the agent programs.
func systemdAutostartUnit(execPath, pathEnv, shellEnv string) string {
	return fmt.Sprintf(`[Unit]
Description=Agent Factory daemon (task scheduler + autoyes)

[Service]
ExecStart=%s --daemon
Restart=on-failure
RestartSec=5
Environment=PATH=%s
Environment=SHELL=%s

[Install]
WantedBy=default.target
`, quoteExecStartPath(execPath), sanitizeEnvValue(pathEnv), sanitizeEnvValue(shellEnv))
}

// launchdAutostartPlist renders the launchd agent that keeps the daemon
// running on macOS. KeepAlive on unsuccessful exit mirrors the systemd
// Restart=on-failure behavior; logPath captures crashes that happen before
// the daemon's own logging is up.
func launchdAutostartPlist(execPath, pathEnv, shellEnv, logPath string) string {
	esc := html.EscapeString
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
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, autostartLaunchdLabel, esc(execPath), esc(pathEnv), esc(shellEnv), esc(logPath), esc(logPath))
}

// InstallAutostart registers the daemon for autostart at login: a systemd
// user service on Linux, a launchd agent on macOS. Any daemon started ad hoc
// (by EnsureDaemon) is stopped first so the supervised unit's daemon is the
// only one running. Returns the path of the installed unit file.
func InstallAutostart() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}

	switch runtime.GOOS {
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		dir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create systemd user directory: %w", err)
		}
		unitPath := filepath.Join(dir, autostartUnitName)
		content := systemdAutostartUnit(execPath, os.Getenv("PATH"), os.Getenv("SHELL"))
		if err := config.AtomicWriteFile(unitPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write unit file: %w", err)
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to reload systemd user daemon: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		// Hand over from any ad-hoc daemon to the supervised one.
		if err := StopDaemon(); err != nil {
			return "", fmt.Errorf("failed to stop the running daemon before enabling the service: %w", err)
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", autostartUnitName).CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to enable daemon service: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return unitPath, nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
		configDir, err := config.GetConfigDir()
		if err != nil {
			return "", fmt.Errorf("failed to get config directory: %w", err)
		}
		logPath := filepath.Join(configDir, "daemon-launchd.log")
		content := launchdAutostartPlist(execPath, os.Getenv("PATH"), os.Getenv("SHELL"), logPath)
		// Unload a previous agent first so launchctl load picks up the new file.
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := config.AtomicWriteFile(plistPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write plist file: %w", err)
		}
		// Hand over from any ad-hoc daemon to the supervised one.
		if err := StopDaemon(); err != nil {
			return "", fmt.Errorf("failed to stop the running daemon before loading the agent: %w", err)
		}
		if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to load launch agent: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return plistPath, nil

	default:
		return "", fmt.Errorf("daemon autostart is not supported on %s (the daemon still starts automatically whenever you run af)", runtime.GOOS)
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
