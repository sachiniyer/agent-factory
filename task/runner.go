package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
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

	return config.WithFileLock(path, func() error {
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
		return config.AtomicWriteFile(path, out, 0644)
	})
}

// LoadAndClearPendingInstances reads pending instances written by task runs
// and removes the file. The TUI should call this at startup to merge them in.
func LoadAndClearPendingInstances() ([]session.InstanceData, error) {
	path, err := getPendingInstancesPath()
	if err != nil {
		return nil, err
	}

	var pending []session.InstanceData
	err = config.WithFileLock(path, func() error {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return nil
			}
			return readErr
		}

		if err := json.Unmarshal(raw, &pending); err != nil {
			return err
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove pending instances file: %v", err)
		}
		return nil
	})
	return pending, err
}

// WaitForReady polls the instance's tmux pane until the program shows its
// input prompt (e.g. Claude Code's ">" prompt) or trust prompt, or times out after 60 seconds.
func WaitForReady(instance *session.Instance) error {
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, err := instance.Preview()
			if err != nil {
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
			} else {
				log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			}
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

// RunTask executes a task by creating a new instance,
// sending the prompt, and registering it in the application state.
func RunTask(taskID string) error {
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
	lockPath := filepath.Join(lockDir, "task-"+taskID+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("another run is already active for task %s: %w", taskID, err)
	}
	defer os.Remove(lockPath)
	defer lockFile.Close()

	// Load the task.
	t, err := GetTask(taskID)
	if err != nil {
		return fmt.Errorf("failed to load task: %w", err)
	}

	if !t.Enabled {
		return fmt.Errorf("task %s is disabled", taskID)
	}

	// Validate project path.
	if !git.IsGitRepo(t.ProjectPath) {
		return fmt.Errorf("project path %s is not a valid git repository", t.ProjectPath)
	}

	cfg := config.LoadConfig()

	title := t.Name
	if title == "" {
		title = fmt.Sprintf("task-%s", t.ID)
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.ProjectPath,
		Program: t.Program,
	})
	if err != nil {
		return fmt.Errorf("failed to create instance: %w", err)
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

	if err := StartAndSendPrompt(instance, t.Prompt); err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}
	instance.SetStatus(session.Running)

	// Instance is successfully handed off, don't kill it on return.
	started = false

	// Write instance to a separate pending file to avoid racing with the
	// daemon/TUI which also read-modify-write state.json concurrently.
	if err := appendPendingInstance(instance.ToInstanceData()); err != nil {
		return fmt.Errorf("failed to save pending instance: %w", err)
	}

	// Create a board task linked to the new instance.
	repo, repoErr := config.RepoFromPath(t.ProjectPath)
	if repoErr == nil {
		taskTitle := t.Name
		if taskTitle == "" {
			taskTitle = title
		}
		if err := board.AddAndLinkTaskForRepo(repo, taskTitle, "in_progress", title); err != nil {
			log.ErrorLog.Printf("failed to save board task: %v", err)
		}
	}

	// Launch daemon for autoyes if configured.
	if cfg.AutoYes {
		instance.AutoYes = true
		if err := daemon.LaunchDaemon(); err != nil {
			log.ErrorLog.Printf("failed to launch daemon: %v", err)
		}
	}

	// Update task status.
	now := time.Now()
	t.LastRunAt = &now
	t.LastRunStatus = "started"
	if err := UpdateTask(*t); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}

	log.InfoLog.Printf("task %s started successfully as instance %s", taskID, title)
	return nil
}
