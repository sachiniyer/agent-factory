//go:build linux

package task

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func getUnitName(t Task) string {
	return "agent-factory-task-" + t.ID
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

// generateServiceContent builds the systemd service unit file content.
func generateServiceContent(unitName, execPath, taskID, projectPath, pathEnv, homeEnv, shellEnv, termEnv string) string {
	return fmt.Sprintf(`[Unit]
Description=Agent Factory task %s

[Service]
Type=oneshot
ExecStart="%s" task run %s
Environment="PATH=%s"
Environment="HOME=%s"
Environment="SHELL=%s"
Environment="TERM=%s"
WorkingDirectory="%s"
`, unitName, execPath, taskID, pathEnv, homeEnv, shellEnv, termEnv, projectPath)
}

func InstallScheduler(t Task) error {
	unitName := getUnitName(t)

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

	serviceContent := generateServiceContent(unitName, execPath, t.ID, t.ProjectPath, pathEnv, homeEnv, shellEnv, termEnv)

	servicePath := filepath.Join(dir, unitName+".service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	onCalendar, err := CronToOnCalendar(t.CronExpr)
	if err != nil {
		return fmt.Errorf("failed to convert cron expression: %w", err)
	}

	timerContent := fmt.Sprintf(`[Unit]
Description=Timer for Agent Factory task %s

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

func RemoveScheduler(t Task) error {
	unitName := getUnitName(t)

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
