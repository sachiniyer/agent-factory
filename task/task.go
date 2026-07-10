package task

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const tasksFileName = "tasks.json"

// taskIDPattern restricts a task ID to characters that are safe to use as a
// single path segment. Legitimate IDs from GenerateID are 8 lowercase hex
// characters; the wider class accommodates any future ID scheme while
// preventing path-traversal segments like "..", "/", or "\".
var taskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// maxTaskIDLength caps the size of an accepted task ID. Legitimate IDs are
// 8 chars; the cap is loose enough for future schemes while bounding the
// size of values that flow into filesystem paths and error messages.
const maxTaskIDLength = 128

// ValidateTaskID enforces the shape of a task identifier before it is used
// to construct filesystem paths (lock files, log files, scheduler units).
// Returns an error when the id is empty, exceeds maxTaskIDLength, or
// contains any character outside [a-zA-Z0-9_-] — in particular "." (used
// in traversal), "/", or "\". Mirrors config.ValidateRepoID.
func ValidateTaskID(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("invalid task id: empty")
	}
	if len(taskID) > maxTaskIDLength {
		return fmt.Errorf("invalid task id: length %d exceeds maximum %d", len(taskID), maxTaskIDLength)
	}
	if !taskIDPattern.MatchString(taskID) {
		return fmt.Errorf("invalid task id: must match %s", taskIDPattern.String())
	}
	return nil
}

type Task struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt"`
	// Exactly one of CronExpr (time trigger) and WatchCmd (event trigger) is
	// set on an enabled task — see ValidateTrigger. A watch task runs WatchCmd
	// as a long-lived script under the daemon; each stdout line it emits is one
	// event (#782 phase 2).
	CronExpr string `json:"cron_expr,omitempty"`
	WatchCmd string `json:"watch_cmd,omitempty"`
	// TargetSession routes deliveries into an existing session by title
	// (auto-created with ProjectPath/Program if missing). Empty keeps the
	// historical behavior of creating a fresh session per run.
	TargetSession string     `json:"target_session,omitempty"`
	ProjectPath   string     `json:"project_path"`
	Program       string     `json:"program"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
}

// IsWatch reports whether the task is event-triggered (WatchCmd) rather than
// time-triggered (CronExpr).
func (t Task) IsWatch() bool {
	return strings.TrimSpace(t.WatchCmd) != ""
}

// ValidateTrigger enforces the trigger contract from #782: a task with both
// CronExpr and WatchCmd set is always invalid (ambiguous), and an enabled
// task must have exactly one of the two. A disabled task with neither is
// tolerated as a draft so hand-edited or legacy records never brick the
// store.
//
// An enabled cron task must additionally carry a non-empty prompt (#1000). A
// cron fire has no event line to fall back to, so the runtime skips an empty
// prompt and produces a session that silently does nothing. Watch tasks are
// exempt: an empty prompt defaults to the emitted line (see RenderWatchPrompt).
// Disabled drafts are still tolerated regardless of prompt.
func (t Task) ValidateTrigger() error {
	hasCron := strings.TrimSpace(t.CronExpr) != ""
	hasWatch := strings.TrimSpace(t.WatchCmd) != ""
	if hasCron && hasWatch {
		return fmt.Errorf("task %s sets both cron_expr and watch_cmd; exactly one trigger is allowed", t.ID)
	}
	if t.Enabled && !hasCron && !hasWatch {
		return fmt.Errorf("task %s is enabled but has neither cron_expr nor watch_cmd; exactly one trigger is required", t.ID)
	}
	if t.Enabled && hasCron && strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("task %s is an enabled cron task but has an empty prompt; a prompt is required", t.ID)
	}
	return nil
}

// watchLinePlaceholder is the template token in a watch task's prompt that is
// replaced with the emitted stdout line at delivery time.
const watchLinePlaceholder = "{{line}}"

// RenderWatchPrompt renders the prompt for one watch event. An empty (or
// whitespace-only) prompt defaults to the raw emitted line; otherwise every
// {{line}} occurrence in the prompt is substituted with the line.
func RenderWatchPrompt(prompt, line string) string {
	if strings.TrimSpace(prompt) == "" {
		return line
	}
	return strings.ReplaceAll(prompt, watchLinePlaceholder, line)
}

// getTasksPathFn is the function used to resolve the tasks file path.
// It can be overridden in tests.
var getTasksPathFn = getTasksPath

func getTasksPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	return filepath.Join(configDir, tasksFileName), nil
}

func LoadTasks() ([]Task, error) {
	path, err := getTasksPathFn()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return []Task{}, nil
		}
		return nil, fmt.Errorf("failed to read tasks file: %w", err)
	}

	data, _, err := loadAndMigrateTasksFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tasks file: %w", err)
	}

	return tasksFromSchemaBytes(data)
}

// loadTasksLocked reads tasks while the caller already holds path's file lock.
// It must not call LoadTasks because LoadTasks may migrate and acquire the same
// lock. If a legacy array sneaks in between the pre-lock migration and this
// read, saveTasks will still write the updated v1 envelope.
func loadTasksLocked(path string) ([]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Task{}, nil
		}
		return nil, fmt.Errorf("failed to read tasks file: %w", err)
	}
	migrated, _, err := migrateTasksSchemaBytes(data, path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tasks file: %w", err)
	}
	return tasksFromSchemaBytes(migrated)
}

func ensureTasksSchemaMigrated(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read tasks file: %w", err)
	}
	_, _, err := loadAndMigrateTasksFile(path)
	if err != nil {
		return fmt.Errorf("failed to parse tasks file: %w", err)
	}
	return nil
}

// saveTasks writes tasks without locking. Must be called from within WithFileLock.
func saveTasks(tasks []Task) error {
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tasks: %w", err)
	}
	enveloped, err := marshalTasksEnvelope(data)
	if err != nil {
		return fmt.Errorf("failed to marshal tasks envelope: %w", err)
	}
	return config.AtomicWriteFile(path, enveloped, 0644)
}

func AddTask(t Task) error {
	if err := ValidateTaskID(t.ID); err != nil {
		return err
	}
	if err := t.ValidateTrigger(); err != nil {
		return err
	}
	// Empty Program means "fall back to the configured default_program at
	// run time"; only validate when an explicit per-task override was set.
	if t.Program != "" {
		if err := config.ValidateProgramEnum("task program", "task program", t.Program, ""); err != nil {
			return err
		}
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}
		tasks = append(tasks, t)
		return saveTasks(tasks)
	})
}

func RemoveTask(id string) error {
	if err := ValidateTaskID(id); err != nil {
		return err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}

		filtered := make([]Task, 0, len(tasks))
		found := false
		for _, t := range tasks {
			if t.ID == id {
				found = true
				continue
			}
			filtered = append(filtered, t)
		}

		if !found {
			return fmt.Errorf("task with id %q not found", id)
		}

		return saveTasks(filtered)
	})
}

func GetTask(id string) (*Task, error) {
	if err := ValidateTaskID(id); err != nil {
		return nil, err
	}
	tasks, err := LoadTasks()
	if err != nil {
		return nil, err
	}

	for _, t := range tasks {
		if t.ID == id {
			return &t, nil
		}
	}

	return nil, fmt.Errorf("task with id %q not found", id)
}

// randReader is the entropy source for GenerateID. It is a package variable so
// tests can substitute a failing reader; production reads from crypto/rand.
var randReader io.Reader = rand.Reader

// GenerateID returns a random 8-character (4-byte) hex task ID. It returns an
// error when the system entropy source is unavailable instead of silently
// emitting the all-zero "00000000" ID: task IDs are the handle users pass to
// `af tasks get/remove/update <id>`, and duplicate IDs make GetTask/RemoveTask/
// UpdateTask (all first-match) operate on the wrong task — silent data loss.
// Callers must fail the operation loudly rather than persist a zero/colliding
// ID. See #897.
func GenerateID() (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(randReader, b); err != nil {
		return "", fmt.Errorf("failed to generate random task ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// LoadTasksForCurrentRepo returns only tasks whose ProjectPath
// matches the current git repository root.
func LoadTasksForCurrentRepo() ([]Task, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	return LoadTasksForRepo(repo.Root)
}

// LoadTasksForRepo returns only tasks whose ProjectPath matches repoRoot (the
// repo's main-worktree root). It is the repo-scoped loader the in-place project
// switch (#1461) uses to repopulate the automations strip for the newly active
// project, without re-deriving the repo from cwd.
func LoadTasksForRepo(repoRoot string) ([]Task, error) {
	all, err := LoadTasks()
	if err != nil {
		return nil, err
	}
	var filtered []Task
	for _, t := range all {
		if t.ProjectPath == repoRoot {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

// UpdateTaskStatus updates only the LastRunAt and LastRunStatus fields of the
// task with the given ID. Unlike UpdateTask, it does not re-validate other
// fields (notably Program), so pre-existing tasks whose Program value would
// fail current enum validation can still have their run status bumped by the
// scheduler and TUI dispatch paths. Returns an error if no task with the given
// ID exists.
//
// A nil lastRunAt means "leave LastRunAt untouched" — only LastRunStatus is
// written. Callers that record a supervision-status change (not an event
// delivery) pass nil so a concurrent writer's newer LastRunAt is never reverted
// by a value the caller read outside the file lock (#1215).
func UpdateTaskStatus(taskID string, lastRunAt *time.Time, lastRunStatus string) error {
	if err := ValidateTaskID(taskID); err != nil {
		return err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}

		found := false
		for i := range tasks {
			if tasks[i].ID == taskID {
				// nil means "preserve the on-disk LastRunAt": a status-only
				// update must not clobber a newer event-delivery timestamp that
				// a concurrent writer committed while this caller held a stale
				// copy (#1215).
				if lastRunAt != nil {
					tasks[i].LastRunAt = lastRunAt
				}
				tasks[i].LastRunStatus = lastRunStatus
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("task with id %q not found", taskID)
		}

		return saveTasks(tasks)
	})
}

func UpdateTask(t Task) error {
	if err := ValidateTaskID(t.ID); err != nil {
		return err
	}
	if err := t.ValidateTrigger(); err != nil {
		return err
	}
	// Empty Program means "fall back to the configured default_program at
	// run time"; only validate when an explicit per-task override was set.
	if t.Program != "" {
		if err := config.ValidateProgramEnum("task program", "task program", t.Program, ""); err != nil {
			return err
		}
	}
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}

		found := false
		for i, existing := range tasks {
			if existing.ID == t.ID {
				// Preserve scheduler/system-managed fields from the freshly
				// loaded record. UpdateTask is a user-edit path (name, prompt,
				// cron/watch_cmd, target_session, program, enabled); its caller may carry a stale copy of
				// LastRunAt/LastRunStatus read before a concurrent scheduler run
				// or manual trigger updated them (TOCTOU via GetTask outside the
				// lock, or a stale TUI cache). Re-applying the caller's struct
				// wholesale would clobber those fresher values. CreatedAt is
				// immutable. UpdateTaskStatus remains the canonical path for the
				// scheduler-owned status fields. See #731.
				t.LastRunAt = existing.LastRunAt
				t.LastRunStatus = existing.LastRunStatus
				t.CreatedAt = existing.CreatedAt
				tasks[i] = t
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("task with id %q not found", t.ID)
		}

		return saveTasks(tasks)
	})
}
