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
//
// The same rules apply to WorkingDirectory=: a raw newline would terminate
// the value and cause systemd to run the task in a truncated path, so this
// helper is reused there as well.
func sanitizeEnvValue(v string) string {
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	return v
}

// generateServiceContent builds the systemd service unit file content.
func generateServiceContent(unitName, execPath, taskID, projectPath, pathEnv, homeEnv, shellEnv, termEnv string) string {
	return fmt.Sprintf(`[Unit]
Description=Agent Factory task %s

[Service]
Type=oneshot
ExecStart=%s task run %s
Environment=PATH=%s
Environment=HOME=%s
Environment=SHELL=%s
Environment=TERM=%s
WorkingDirectory=%s
`, unitName,
		quoteExecStartPath(execPath), taskID,
		sanitizeEnvValue(pathEnv),
		sanitizeEnvValue(homeEnv),
		sanitizeEnvValue(shellEnv),
		sanitizeEnvValue(termEnv),
		sanitizeEnvValue(projectPath))
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
