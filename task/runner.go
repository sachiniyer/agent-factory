package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
			log.WarningLog.Printf("failed to parse pending instances file, discarding: %v", err)
			pending = nil
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.WarningLog.Printf("failed to remove pending instances file: %v", err)
		}
		return nil
	})
	return pending, err
}

// isReadyContent reports whether the captured pane content indicates the
// program is ready for input or is showing a trust prompt that downstream
// handlers know how to dismiss. It recognizes Claude Code's input prompt
// and trust prompt as well as the Aider/Gemini trust prompt
// ("Open documentation url" + "(D)on't ask again").
func isReadyContent(content string) bool {
	if strings.Contains(content, "❯") ||
		strings.Contains(content, "Do you trust") ||
		strings.Contains(content, "new MCP server") {
		return true
	}
	// Aider/Gemini trust prompt. Require both substrings to avoid false
	// positives from documentation links unrelated to the trust prompt.
	if strings.Contains(content, "Open documentation url") &&
		strings.Contains(content, "(D)on't ask again") {
		return true
	}
	return false
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
			if isReadyContent(content) {
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
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another run is already active for task %s", taskID)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

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

	// Apply AutoYes to the in-memory instance before persisting so the
	// flag is part of the saved data the daemon will read back.
	if cfg.AutoYes {
		instance.AutoYes = true
	}

	// Persist the new instance into per-repo instances.json under the
	// file lock. This is the source of truth the daemon reads via
	// storage.LoadInstances; without this write, the daemon never sees
	// scheduled tasks and AutoYes runs hang. Mirrors api/sessions.go.
	repo, err := config.RepoFromPath(t.ProjectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve repo for project path %s: %w", t.ProjectPath, err)
	}
	data := instance.ToInstanceData()
	if err := config.UpdateRepoInstances(repo.ID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	}); err != nil {
		return fmt.Errorf("failed to save instance to per-repo storage: %w", err)
	}

	// Also write to the pending file so the running TUI (which doesn't
	// re-read instances.json on every tick) can merge the new instance
	// into its in-memory list at startup. This is defensive: the daemon
	// reads from per-repo instances.json above, which is the load-bearing
	// path for AutoYes.
	if err := appendPendingInstance(data); err != nil {
		log.WarningLog.Printf("failed to save pending instance: %v", err)
	}

	// Instance is successfully handed off, don't kill it on return.
	started = false

	// Launch daemon for autoyes if configured.
	if cfg.AutoYes {
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
