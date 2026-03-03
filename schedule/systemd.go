//go:build linux

package schedule

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func getUnitName(s Schedule) string {
	return "claude-squad-sched-" + s.ID
}

func getSystemdUserDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create systemd user directory: %w", err)
	}
	return dir, nil
}

func InstallSystemdTimer(s Schedule) error {
	unitName := getUnitName(s)

	dir, err := getSystemdUserDir()
	if err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	pathEnv := os.Getenv("PATH")
	homeEnv := os.Getenv("HOME")
	shellEnv := os.Getenv("SHELL")
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}

	serviceContent := fmt.Sprintf(`[Unit]
Description=Claude Squad scheduled task %s

[Service]
Type=oneshot
ExecStart=%s schedule run %s
Environment="PATH=%s"
Environment="HOME=%s"
Environment="SHELL=%s"
Environment="TERM=%s"
WorkingDirectory=%s
`, unitName, execPath, s.ID, pathEnv, homeEnv, shellEnv, termEnv, s.ProjectPath)

	servicePath := filepath.Join(dir, unitName+".service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	onCalendar, err := CronToOnCalendar(s.CronExpr)
	if err != nil {
		return fmt.Errorf("failed to convert cron expression: %w", err)
	}

	timerContent := fmt.Sprintf(`[Unit]
Description=Timer for Claude Squad scheduled task %s

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
`, unitName, onCalendar)

	timerPath := filepath.Join(dir, unitName+".timer")
	if err := os.WriteFile(timerPath, []byte(timerContent), 0644); err != nil {
		return fmt.Errorf("failed to write timer file: %w", err)
	}

	reloadCmd := exec.Command("systemctl", "--user", "daemon-reload")
	if out, err := reloadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	enableCmd := exec.Command("systemctl", "--user", "enable", "--now", unitName+".timer")
	if out, err := enableCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to enable timer: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func RemoveSystemdTimer(s Schedule) error {
	unitName := getUnitName(s)

	dir, err := getSystemdUserDir()
	if err != nil {
		return err
	}

	// Disable and stop the timer (ignore error if it doesn't exist)
	disableCmd := exec.Command("systemctl", "--user", "disable", "--now", unitName+".timer")
	_ = disableCmd.Run()

	// Remove service file (ignore not exist)
	servicePath := filepath.Join(dir, unitName+".service")
	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove service file: %w", err)
	}

	// Remove timer file (ignore not exist)
	timerPath := filepath.Join(dir, unitName+".timer")
	if err := os.Remove(timerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove timer file: %w", err)
	}

	// Reload daemon
	reloadCmd := exec.Command("systemctl", "--user", "daemon-reload")
	if out, err := reloadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}
