package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// RunTask executes a task by creating a new session via the daemon's
// CreateSession path and recording the run status on the task. It is the
// single firing path for tasks: the in-daemon cron scheduler and the
// `af tasks trigger` CLI both land here. When called from inside the daemon
// the CreateSession RPC loops back through the daemon's own control socket,
// so both callers share one code path.
func RunTask(taskID string) error {
	// Validate the task ID before it flows into any filesystem path. The
	// CLI boundary also validates, but this is the shared chokepoint that
	// protects every caller.
	if err := task.ValidateTaskID(taskID); err != nil {
		return err
	}

	// Load the task first so a nonexistent ID never causes a lock file to
	// be created. The original ordering wrote a lock for any caller-supplied
	// ID before validation (issue #575).
	t, err := task.GetTask(taskID)
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

	// Validate project path. Distinguish a missing git binary from a path that
	// simply is not a repo so the daemon surfaces an actionable error (#737).
	if !git.IsGitInstalled() {
		return fmt.Errorf("git is not installed or could not be found in PATH; install git and ensure it is available in your PATH")
	}
	if !git.IsGitRepo(t.ProjectPath) {
		return fmt.Errorf("project path %s is not a valid git repository", t.ProjectPath)
	}

	baseTitle := task.TaskRunBaseTitle(*t)
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	data, err := CreateSession(CreateSessionRequest{
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
	// — the task already ran via CreateSession, and the stored Program value
	// may predate current enum validation (see #664).
	now := time.Now()
	if err := task.UpdateTaskStatus(taskID, &now, "started"); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}

	log.InfoLog.Printf("task %s started successfully as instance %s", taskID, data.Title)
	return nil
}
