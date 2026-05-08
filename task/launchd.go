//go:build darwin

package task

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

func getLaunchAgentLabel(t Task) string {
	return "com.agent-factory.task-" + t.ID
}

func getLaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}
	return dir, nil
}

func InstallScheduler(t Task) error {
	label := getLaunchAgentLabel(t)

	dir, err := getLaunchAgentsDir()
	if err != nil {
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	scheduleXML, err := cronToLaunchdScheduleXML(t.CronExpr)
	if err != nil {
		return fmt.Errorf("failed to convert cron expression: %w", err)
	}

	pathEnv := os.Getenv("PATH")
	homeEnv := os.Getenv("HOME")
	shellEnv := os.Getenv("SHELL")
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}

	logDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	logPath := filepath.Join(logDir, "task-"+t.ID+".log")

	// Escape all interpolated values for XML safety.
	esc := html.EscapeString
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>task</string>
        <string>run</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HOME</key>
        <string>%s</string>
        <key>SHELL</key>
        <string>%s</string>
        <key>TERM</key>
        <string>%s</string>
    </dict>
%s
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, esc(label), esc(execPath), esc(t.ID), esc(t.ProjectPath),
		esc(pathEnv), esc(homeEnv), esc(shellEnv), esc(termEnv),
		scheduleXML, esc(logPath), esc(logPath))

	plistPath := filepath.Join(dir, label+".plist")

	// Unload existing agent if present (ignore errors).
	unloadCmd := exec.Command("launchctl", "unload", plistPath)
	_ = unloadCmd.Run()

	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	loadCmd := exec.Command("launchctl", "load", plistPath)
	if out, err := loadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to load launch agent: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func RemoveScheduler(t Task) error {
	label := getLaunchAgentLabel(t)

	dir, err := getLaunchAgentsDir()
	if err != nil {
		return err
	}

	plistPath := filepath.Join(dir, label+".plist")

	// Unload the launch agent (ignore error if not loaded).
	unloadCmd := exec.Command("launchctl", "unload", plistPath)
	_ = unloadCmd.Run()

	// Remove plist file.
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}

	return nil
}
