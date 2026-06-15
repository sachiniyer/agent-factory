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

// Indirected so delivery tests can observe the daemon RPCs without dialing —
// or spawning — a real daemon. Both helpers loop back through the daemon's
// own control socket when called from inside the daemon process.
var (
	createSessionForTask = CreateSession
	deliverPromptForTask = DeliverPrompt
)

// deliverTaskPrompt delivers one rendered prompt for a task and returns the
// status string to record on it. With TargetSession empty it creates a fresh
// session per run (the historical task behavior, status "started"). With
// TargetSession set it sends the prompt into that session (status "sent"),
// auto-creating the session with the task's ProjectPath/Program when it does
// not exist yet (Sachin-approved in #782, mirroring `af sessions send-prompt
// --create`). The target session is looked up in the task's own repo so a
// same-titled session in an unrelated repo can never receive the prompt.
func deliverTaskPrompt(t *task.Task, prompt string) (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}

	if t.TargetSession == "" {
		data, err := createSessionForTask(CreateSessionRequest{
			TitleBase: task.TaskRunBaseTitle(*t),
			RepoPath:  t.ProjectPath,
			Program:   t.Program,
			Prompt:    prompt,
			AutoYes:   cfg.AutoYes,
		})
		if err != nil {
			return "", fmt.Errorf("failed to start task session: %w", err)
		}
		log.InfoLog.Printf("task %s started successfully as instance %s", t.ID, data.Title)
		return "started", nil
	}

	// Route through the daemon's serialized create-or-send path. When several
	// tasks fire at the same missing target_session, the daemon creates it once
	// and delivers every prompt in order instead of dropping the losers of the
	// creation race (#865). A Deleting target is surfaced, not silently dropped.
	status, err := deliverPromptForTask(DeliverPromptRequest{
		Title:    t.TargetSession,
		RepoPath: t.ProjectPath,
		Program:  t.Program,
		Prompt:   prompt,
		AutoYes:  cfg.AutoYes,
	})
	if err != nil {
		return "", fmt.Errorf("failed to deliver prompt to target session %q: %w", t.TargetSession, err)
	}
	log.InfoLog.Printf("task %s delivered prompt to target session %q (%s)", t.ID, t.TargetSession, status)
	return status, nil
}

// repoHasSessionTitle reports whether a persisted session with the given
// title exists in the repo. Mirrors api.repoHasInstanceTitle, which daemon/
// cannot import without a cycle.
func repoHasSessionTitle(repoID, title string) (bool, error) {
	data, err := loadRepoInstanceData(repoID)
	if err != nil {
		return false, err
	}
	for i := range data {
		if data[i].Title == title {
			return true, nil
		}
	}
	return false, nil
}

// RunTask executes a cron task by delivering its prompt (create a session,
// or send into TargetSession) and recording the run status. It is the single
// firing path for cron tasks: the in-daemon scheduler and the `af tasks
// trigger` CLI both land here. Watch tasks are refused — they fire from
// their watch command's stdout, and a manual trigger has no event line to
// render the prompt with.
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

	if t.IsWatch() {
		return fmt.Errorf("task %s is a watch task; it fires when its watch command emits output, not on manual trigger", taskID)
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

	status, err := deliverTaskPrompt(t, t.Prompt)
	if err != nil {
		return err
	}

	// Update task status. Use UpdateTaskStatus so we don't re-validate Program
	// — the task already ran via deliverTaskPrompt, and the stored Program
	// value may predate current enum validation (see #664).
	now := time.Now()
	if err := task.UpdateTaskStatus(taskID, &now, status); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}
	return nil
}
