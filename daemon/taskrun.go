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
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// TaskStatusLimitParked is the run status recorded when a task-driven session
// hits a usage-limit wall during startup and is PARKED instead of failed (#1146
// PR4). It is deliberately NOT an "errored:"-prefixed value, so the TUI and task
// history show the run waiting for the limit window to reset — not failed — and
// no failure side-effects fire. The daemon auto-resume scheduler (opt-in) or the
// manual `c` retry re-delivers the stored prompt once the window resets, after
// which the run records its normal completion status.
const TaskStatusLimitParked = "parked: usage limit"

// Indirected so delivery tests can observe the daemon RPCs without dialing —
// or spawning — a real daemon. Both helpers loop back through the daemon's
// own control socket when called from inside the daemon process.
var (
	createSessionForTask = CreateSession
	deliverPromptForTask = DeliverPrompt
)

// cronDeferPollInterval / cronDeferMaxWait bound how a cron fire waits out a
// #1586 deferral (target attached). Cron has no durable event queue the way
// watch does, so a deferred occurrence is caught up by re-attempting on this
// cadence until the user detaches, then force-delivered at the bound so it is
// never silently dropped. Vars so tests can shrink them.
var (
	cronDeferPollInterval = 1 * time.Second
	cronDeferMaxWait      = 5 * time.Minute
)

// deliverTaskPrompt delivers one rendered prompt for a task and returns the
// status string to record on it. With TargetSession empty it creates a fresh
// session per run (the historical task behavior, status "started"). With
// TargetSession set it sends the prompt into that session (status "sent"),
// auto-creating the session with the task's ProjectPath/Program when it does
// not exist yet (Sachin-approved in #782, mirroring `af sessions send-prompt
// --create`). The target session is looked up in the task's own repo so a
// same-titled session in an unrelated repo can never receive the prompt.
//
// deferWhileAttached asks the daemon to hold the send while a TUI is attached
// full-screen to (or interactively focused on) the target, returning
// StatusDeferredAttached instead of pasting into the user's in-progress input
// (#1586). Callers that can catch up a held delivery pass true; a forced final
// attempt passes false.
func deliverTaskPrompt(t *task.Task, prompt string, deferWhileAttached bool) (string, error) {
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
		// The freshly created session hit a usage-limit wall during startup and
		// was parked, not failed (#1146 PR4). Record the parked status so the run
		// is NOT counted as a failure; the resume machinery re-delivers the
		// prompt once the limit window resets.
		if data.Liveness == session.LiveLimitReached {
			log.InfoLog.Printf("task %s parked at a usage limit as instance %s; waiting for the limit window to reset", t.ID, data.Title)
			return TaskStatusLimitParked, nil
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
		// An automated delivery (cron fire or watch event): hold it while a TUI is
		// attached to the target so it never pastes into and submits the user's
		// in-progress input (#1586). The caller decides how a hold is handled.
		DeferWhileAttached: deferWhileAttached,
	})
	if err != nil {
		return "", fmt.Errorf("failed to deliver prompt to target session %q: %w", t.TargetSession, err)
	}
	log.InfoLog.Printf("task %s delivered prompt to target session %q (%s)", t.ID, t.TargetSession, status)
	return status, nil
}

// deliverCronTaskPrompt delivers a cron task's prompt, waiting out a #1586
// deferral (a TUI is attached to the target) so the occurrence is caught up on
// detach rather than silently skipped — cron has no durable event queue the way
// watch does. It re-attempts on cronDeferPollInterval until the user detaches,
// bounded by cronDeferMaxWait so a session left attached indefinitely can't park
// the fire forever; at the bound it delivers once with deferral OFF so the
// occurrence is never dropped. Overlapping fires of the same task are coalesced
// by RunTask's per-task flock, so a long attach yields exactly one catch-up
// delivery, not a stacked burst.
func deliverCronTaskPrompt(t *task.Task, prompt string) (string, error) {
	status, err := deliverTaskPrompt(t, prompt, true)
	if err != nil || status != StatusDeferredAttached {
		return status, err
	}
	deadline := time.Now().Add(cronDeferMaxWait)
	for {
		time.Sleep(cronDeferPollInterval)
		// Once past the bound, force the delivery (deferral off) so an
		// indefinitely-attached target never drops the occurrence.
		deferWhileAttached := time.Now().Before(deadline)
		status, err = deliverTaskPrompt(t, prompt, deferWhileAttached)
		if err != nil || status != StatusDeferredAttached {
			return status, err
		}
	}
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
func RunTask(taskID string) (err error) {
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

	// This NON-blocking lock is what makes overlapping same-task fires during a
	// #1586 delivery defer INTENTIONALLY coalesce to a single delivery. While the
	// user is attached to the target, the holding fire parks in
	// deliverCronTaskPrompt waiting for detach; every other fire that lands during
	// that wait hits LOCK_NB, returns "another run is already active", and exits
	// without queuing. So a cron firing more often than the attach lasts delivers
	// exactly ONE prompt on detach, not one per skipped occurrence. This is
	// deliberate and desirable: a cron prompt is a fixed, idempotent string (the
	// task's Prompt), so N identical "run nightly" prompts arriving in a burst the
	// instant the user detaches would be pure duplicate noise — one catch-up is
	// the right behavior. (Watch events, which carry distinct per-event {{line}}
	// payloads, are NOT coalesced: they each queue durably and replay in order.)

	// Once this fire holds the lock it owns the task's run status: every
	// failure from here on — git missing, project path not a repo, or a
	// delivery error — must be recorded so a cron task's LastRunStatus
	// reflects the failure instead of going stale. Previously only the success
	// path reached UpdateTaskStatus, so a bad project path left the TUI showing
	// the prior run forever while the scheduler merely logged the error (#924).
	// The success path writes its own status below; this defer fires only when
	// err is non-nil, so the status is never double-written. The "errored:"
	// prefix matches the watcher convention the TUI keys on (#797).
	defer func() {
		if err == nil {
			return
		}
		now := time.Now()
		if uerr := task.UpdateTaskStatus(taskID, &now, "errored: "+err.Error()); uerr != nil {
			log.ErrorLog.Printf("failed to record errored status for task %s: %v", taskID, uerr)
		}
	}()

	// Validate project path. Distinguish a missing git binary from a path that
	// simply is not a repo so the daemon surfaces an actionable error (#737).
	if !git.IsGitInstalled() {
		return fmt.Errorf("git is not installed or could not be found in PATH; install git and ensure it is available in your PATH")
	}
	if !git.IsGitRepo(t.ProjectPath) {
		return fmt.Errorf("project path %s is not a valid git repository", t.ProjectPath)
	}

	status, err := deliverCronTaskPrompt(t, t.Prompt)
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
