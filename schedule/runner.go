package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-squad/config"
	"claude-squad/daemon"
	"claude-squad/log"
	"claude-squad/session"
	"claude-squad/session/git"
)

const pendingInstancesFileName = "pending_instances.json"

func getPendingInstancesPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, pendingInstancesFileName), nil
}

// appendPendingInstance appends an instance to the pending_instances.json file.
// This file is used by the runner to avoid racing with the daemon on state.json.
func appendPendingInstance(data session.InstanceData) error {
	path, err := getPendingInstancesPath()
	if err != nil {
		return err
	}

	var pending []session.InstanceData
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &pending); err != nil {
			log.WarningLog.Printf("failed to parse pending instances file, starting fresh: %v", err)
			pending = nil
		}
	}
	pending = append(pending, data)

	out, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// LoadAndClearPendingInstances reads pending instances written by scheduled runs
// and removes the file. The TUI should call this at startup to merge them in.
func LoadAndClearPendingInstances() ([]session.InstanceData, error) {
	path, err := getPendingInstancesPath()
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var pending []session.InstanceData
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove pending instances file: %v", err)
	}
	return pending, nil
}

// WaitForReady polls the instance's tmux pane until the program shows its
// input prompt (e.g. Claude Code's "❯" prompt) or trust prompt, or times out after 60 seconds.
func WaitForReady(instance *session.Instance) error {
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, _ := instance.Preview()
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return fmt.Errorf("timed out waiting for program to start (60s)")
		case <-ticker.C:
			content, err := instance.Preview()
			if err != nil {
				continue
			}
			if strings.Contains(content, "❯") || strings.Contains(content, "Do you trust") {
				return nil
			}
		}
	}
}

// RunScheduledTask executes a scheduled task by creating a new instance,
// sending the prompt, and registering it in the application state.
func RunScheduledTask(scheduleID string) error {
	log.Initialize(false)
	defer log.Close()

	// Create lock file to prevent overlapping runs.
	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	lockDir := filepath.Join(configDir, "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}
	lockPath := filepath.Join(lockDir, "schedule-"+scheduleID+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("another run is already active for schedule %s: %w", scheduleID, err)
	}
	defer os.Remove(lockPath)
	defer lockFile.Close()

	// Load the schedule.
	s, err := GetSchedule(scheduleID)
	if err != nil {
		return fmt.Errorf("failed to load schedule: %w", err)
	}

	if !s.Enabled {
		return fmt.Errorf("schedule %s is disabled", scheduleID)
	}

	// Validate project path.
	if !git.IsGitRepo(s.ProjectPath) {
		return fmt.Errorf("project path %s is not a valid git repository", s.ProjectPath)
	}

	cfg := config.LoadConfig()

	title := fmt.Sprintf("sched-%s-%s", s.ID, time.Now().Format("20060102-150405"))

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    s.ProjectPath,
		Program: s.Program,
	})
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
	}

	if err := instance.Start(true); err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	// If anything fails after Start(), kill the instance to avoid orphaned resources.
	started := true
	defer func() {
		if started {
			if killErr := instance.Kill(); killErr != nil {
				log.ErrorLog.Printf("failed to kill orphaned instance %s: %v", title, killErr)
			}
		}
	}()

	// Wait for the program to be ready before sending the prompt.
	// Claude Code (and similar tools) take a few seconds to initialize.
	if err := WaitForReady(instance); err != nil {
		return fmt.Errorf("program did not become ready: %w", err)
	}

	// Check for and dismiss the trust prompt if present.
	if instance.CheckAndHandleTrustPrompt() {
		log.InfoLog.Printf("trust prompt detected and dismissed, waiting for ready again")
		time.Sleep(1 * time.Second)
		if err := WaitForReady(instance); err != nil {
			return fmt.Errorf("program did not become ready after trust prompt: %w", err)
		}
	}

	// Use SendPromptCommand (tmux send-keys) instead of SendPrompt (PTY write)
	// for reliability in headless/scheduled runs.
	if err := instance.SendPromptCommand(s.Prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}

	// Instance is successfully handed off, don't kill it on return.
	started = false

	// Write instance to a separate pending file to avoid racing with the
	// daemon/TUI which also read-modify-write state.json concurrently.
	if err := appendPendingInstance(instance.ToInstanceData()); err != nil {
		return fmt.Errorf("failed to save pending instance: %w", err)
	}

	// Launch daemon for autoyes if configured.
	if cfg.AutoYes {
		instance.AutoYes = true
		if err := daemon.LaunchDaemon(); err != nil {
			log.ErrorLog.Printf("failed to launch daemon: %v", err)
		}
	}

	// Update schedule status.
	now := time.Now()
	s.LastRunAt = &now
	s.LastRunStatus = "started"
	if err := UpdateSchedule(*s); err != nil {
		log.ErrorLog.Printf("failed to update schedule status: %v", err)
	}

	log.InfoLog.Printf("scheduled task %s started successfully as instance %s", scheduleID, title)
	return nil
}
