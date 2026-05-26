package task

import (
	"encoding/json"
	"errors"
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
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const pendingInstancesFileName = "pending_instances.json"

var (
	waitForReadyTimeout      = 60 * time.Second
	waitForReadyPollInterval = 500 * time.Millisecond
)

func getPendingInstancesPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, pendingInstancesFileName), nil
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
	timeout := time.After(waitForReadyTimeout)
	ticker := time.NewTicker(waitForReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, err := instance.Preview()
			if err != nil {
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
				return formatWaitForReadyTimeoutError(waitForReadyTimeout, "")
			}
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return formatWaitForReadyTimeoutError(waitForReadyTimeout, content)
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

// formatWaitForReadyTimeoutError builds the user-facing timeout error. When
// the captured pane content is non-empty, the error body carries a trimmed
// snippet of the last few lines so users see what the agent was doing instead
// of an opaque "timed out" message. See sachiniyer/agent-factory#502.
func formatWaitForReadyTimeoutError(timeout time.Duration, content string) error {
	base := fmt.Sprintf("timed out waiting for program to start (%s)", timeout)
	snippet := trimPaneSnippet(content)
	if snippet == "" {
		return errors.New(base)
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\nlast pane content:")
	for _, line := range strings.Split(snippet, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return errors.New(b.String())
}

// trimPaneSnippet returns at most the last 5 non-empty trailing lines of the
// captured pane content, capped at 400 bytes. ANSI escape sequences are left
// intact — keeping the snippet short matters more than stripping them.
func trimPaneSnippet(content string) string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

// RunTask executes a task by creating a new instance,
// sending the prompt, and registering it in the application state.
func RunTask(taskID string) error {
	log.Initialize(false)
	defer log.Close()

	// Validate the task ID before it flows into any filesystem path. The
	// CLI boundary also validates, but RunTask is exposed via `af task run`
	// (the hidden scheduler entry point) and via the API, so this is the
	// shared chokepoint that protects every caller.
	if err := ValidateTaskID(taskID); err != nil {
		return err
	}

	// Load the task first so a nonexistent ID never causes a lock file to
	// be created. The original ordering wrote a lock for any caller-supplied
	// ID before validation (issue #575).
	t, err := GetTask(taskID)
	if err != nil {
		return fmt.Errorf("failed to load task: %w", err)
	}

	if !t.Enabled {
		return fmt.Errorf("task %s is disabled", taskID)
	}

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
	// Defense in depth: even after ValidateTaskID, confirm the joined path
	// remains inside lockDir. ValidateTaskID already rejects "..", "/",
	// and "\", so this is a belt-and-suspenders check matching the
	// config.repoInstancesPath pattern.
	cleanLockDir := filepath.Clean(lockDir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(lockPath), cleanLockDir) {
		return fmt.Errorf("invalid task id: resolved lock path escapes locks directory")
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another run is already active for task %s", taskID)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Validate project path.
	if !git.IsGitRepo(t.ProjectPath) {
		return fmt.Errorf("project path %s is not a valid git repository", t.ProjectPath)
	}

	baseTitle := TaskRunBaseTitle(*t)
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	data, err := daemon.CreateSession(daemon.CreateSessionRequest{
		TitleBase: baseTitle,
		RepoPath:  t.ProjectPath,
		Program:   t.Program,
		Prompt:    t.Prompt,
		AutoYes:   cfg.AutoYes,
	})
	if err != nil {
		return fmt.Errorf("failed to start task session: %w", err)
	}

	// Update task status. Use UpdateTaskStatus so we don't re-validate Program
	// — the task already ran via daemon.CreateSession, and the stored Program
	// value may predate current enum validation (see #664).
	now := time.Now()
	if err := UpdateTaskStatus(taskID, &now, "started"); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}

	log.InfoLog.Printf("task %s started successfully as instance %s", taskID, data.Title)
	return nil
}

func appendTaskRunnerInstanceFn(data session.InstanceData) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		for i := range existing {
			if existing[i].Title == data.Title {
				return nil, fmt.Errorf("session with title %q already exists", data.Title)
			}
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	}
}

// TaskRunBaseTitle returns the preferred title for a task-created session.
func TaskRunBaseTitle(t Task) string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("task-%s", t.ID)
}

// NextTaskRunTitle chooses a repo-scoped title for a task run that will not
// collide with persisted sessions or an already-live tmux session. Recurring
// tasks can fire while a previous run is still around, so task sessions cannot
// use the static task name blindly.
func NextTaskRunTitle(repoID, repoPath, baseTitle, program string) (string, error) {
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		return "", err
	}

	var title string
	if err := config.WithFileLock(path, func() error {
		raw, err := config.LoadRepoInstances(repoID)
		if err != nil {
			return err
		}

		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("failed to parse existing instances: %w", err)
		}

		used := make(map[string]bool, len(existing))
		for _, data := range existing {
			used[data.Title] = true
		}

		for i := 1; i <= 10000; i++ {
			candidate := baseTitle
			if i > 1 {
				candidate = fmt.Sprintf("%s-%d", baseTitle, i)
			}
			if used[candidate] {
				continue
			}
			if tmux.NewTmuxSessionForRepo(candidate, repoPath, program).DoesSessionExist() {
				continue
			}
			title = candidate
			return nil
		}
		return fmt.Errorf("could not find an available title for %q", baseTitle)
	}); err != nil {
		return "", err
	}

	return title, nil
}
