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
	TargetSession string `json:"target_session,omitempty"`
	// MaxConcurrentRuns caps how many sessions this watch task may have in flight
	// at once (#1892). Zero — the default — means unlimited, preserving the
	// historical behavior for every task written before this field existed; a
	// cap is opt-in. Events over the cap are queued durably in FIFO order and
	// delivered as slots free rather than dropped on admission — subject to the
	// event queue's own retention bounds, which every queued event shares.
	//
	// It applies only to a watch task that creates a session per event (see
	// ValidateTrigger): a target_session task delivers into one session, so its
	// deliveries already serialize, and overlapping cron fires already coalesce on
	// RunTask's per-task lock. omitempty + additive: no tasks schema bump.
	MaxConcurrentRuns int        `json:"max_concurrent_runs,omitempty"`
	ProjectPath       string     `json:"project_path"`
	Program           string     `json:"program"`
	Enabled           bool       `json:"enabled"`
	CreatedAt         time.Time  `json:"created_at"`
	LastRunAt         *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus     string     `json:"last_run_status,omitempty"`
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
	// A concurrency cap is meaningful only where sessions can actually pile up:
	// a watch task that creates one per event (#1892). Rejecting it elsewhere is
	// deliberate — accepting it would leave a setting that reads as enforced but
	// silently does nothing, which is the gotcha class this repo designs away.
	if t.MaxConcurrentRuns < 0 {
		return fmt.Errorf("task %s has a negative max_concurrent_runs (%d); use 0 for unlimited or a positive cap", t.ID, t.MaxConcurrentRuns)
	}
	if t.MaxConcurrentRuns > 0 {
		if !hasWatch {
			return fmt.Errorf("task %s sets max_concurrent_runs but is not a watch task; the cap bounds a watch task's in-flight sessions, and overlapping cron fires already coalesce", t.ID)
		}
		if strings.TrimSpace(t.TargetSession) != "" {
			return fmt.Errorf("task %s sets both max_concurrent_runs and target_session; deliveries into one session already serialize, so drop the cap or drop the target session", t.ID)
		}
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
	// Normalize before validating so validation judges exactly what will be
	// stored — a whitespace-only target session must not validate as "no target
	// session" and then behave as one at delivery time (#1892).
	t.normalizeTargetSession()
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

// DeleteAllTasks removes the entire task store, leaving zero scheduled
// cron/watch tasks. It is the wipe primitive for `af reset` (#1736): the whole
// file is removed rather than emptied, and LoadTasks treats a missing file as
// an empty list, so the next daemon start comes up with no schedules.
// Idempotent — a missing store is a clean no-op, so a second `af reset` does
// not error.
//
// The caller (`af reset`) stops the daemon first, so no live writer holds the
// store; taking the file lock still guards against a concurrent CLI writer.
func DeleteAllTasks() error {
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	return config.WithFileLock(path, func() error {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove tasks file: %w", err)
		}
		return nil
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

// TaskUpdate is a field-level patch for UpdateTask (#1700). Each non-nil field
// replaces that field on the freshly-loaded record under the file lock; a nil
// field is left exactly as stored. Because the write carries ONLY the fields the
// caller changed — never a full, possibly-stale copy — a single-field edit (the
// enable/disable toggle sends just Enabled) is structurally incapable of
// clobbering a concurrent edit another client made to a different field.
//
// Only the user-editable fields are patchable. The scheduler-owned LastRunAt/
// LastRunStatus and the immutable CreatedAt never appear here — UpdateTaskStatus
// stays their canonical writer (#731/#1215), and preserving them is now inherent
// to the merge (the record starts from the on-disk copy).
//
// The json tags define the HTTP JSON body shape for the daemon's /v1/UpdateTask
// route; a nil pointer serializes as an absent key (omitempty), so the wire form
// carries exactly the changed fields. The net/rpc gob control socket the CLI
// uses goes through the same JSON encoding via GobEncode/GobDecode below — see
// there for why plain gob would be lossy for this type.
type TaskUpdate struct {
	Name          *string `json:"name,omitempty"`
	Prompt        *string `json:"prompt,omitempty"`
	CronExpr      *string `json:"cron_expr,omitempty"`
	WatchCmd      *string `json:"watch_cmd,omitempty"`
	TargetSession *string `json:"target_session,omitempty"`
	// MaxConcurrentRuns patches the watch-task concurrency cap (#1892). A pointer
	// because 0 is a meaningful value ("unlimited"), not "unchanged" — the same
	// nil-vs-zero distinction Enabled and TargetSession rely on, and the reason
	// this type needs the JSON gob codec below.
	MaxConcurrentRuns *int    `json:"max_concurrent_runs,omitempty"`
	ProjectPath       *string `json:"project_path,omitempty"`
	Program           *string `json:"program,omitempty"`
	Enabled           *bool   `json:"enabled,omitempty"`
}

// GobEncode/GobDecode route TaskUpdate through JSON on the net/rpc gob control
// socket the CLI uses (daemon.UpdateTask → callDaemon). This is REQUIRED for
// correctness, not an optimization: gob elides a struct field holding its zero
// value, and — fatally here — a *bool pointing at false (or a *string at "", or
// an *int at 0) is followed to that zero and dropped, so the pointer decodes back
// as nil. That would silently turn `af tasks update --enabled false`, the
// trigger-clearing WatchCmd:"" / CronExpr:"" patches, and `--max-concurrent-runs
// 0` (revert to unlimited, #1892) into no-ops. JSON preserves the exact
// nil-vs-non-nil-zero-pointer distinction (omitempty omits ONLY a nil pointer, so
// a non-nil &false serializes as `false`), so this round-trip is lossless.
func (u TaskUpdate) GobEncode() ([]byte, error) {
	return json.Marshal(u)
}

func (u *TaskUpdate) GobDecode(data []byte) error {
	return json.Unmarshal(data, u)
}

// IsEmpty reports whether the patch changes no field. An empty patch is a
// well-formed no-op: UpdateTask still validates and returns the record but
// writes nothing new.
func (u TaskUpdate) IsEmpty() bool {
	return u.Name == nil && u.Prompt == nil && u.CronExpr == nil &&
		u.WatchCmd == nil && u.TargetSession == nil && u.MaxConcurrentRuns == nil &&
		u.ProjectPath == nil && u.Program == nil && u.Enabled == nil
}

// apply merges the non-nil fields of u onto t and returns the result. It never
// touches CreatedAt/LastRunAt/LastRunStatus, so a merge onto the freshly-loaded
// record preserves those scheduler-owned values automatically.
func (u TaskUpdate) apply(t Task) Task {
	if u.Name != nil {
		t.Name = *u.Name
	}
	if u.Prompt != nil {
		t.Prompt = *u.Prompt
	}
	if u.CronExpr != nil {
		t.CronExpr = *u.CronExpr
	}
	if u.WatchCmd != nil {
		t.WatchCmd = *u.WatchCmd
	}
	if u.TargetSession != nil {
		t.TargetSession = *u.TargetSession
		// Normalize on the way in so the cap rules below, ValidateTrigger, and
		// deliverTaskPrompt all judge the same value (#1892).
		t.normalizeTargetSession()
	}
	if u.MaxConcurrentRuns != nil {
		t.MaxConcurrentRuns = *u.MaxConcurrentRuns
	}
	if u.ProjectPath != nil {
		t.ProjectPath = *u.ProjectPath
	}
	if u.Program != nil {
		t.Program = *u.Program
	}
	if u.Enabled != nil {
		t.Enabled = *u.Enabled
	}
	// Drop a cap the merged record can no longer carry, unless this patch set it
	// explicitly (#1892). A partial patch that only moves the trigger or the
	// delivery mode — which is every non-CLI writer, since DiffTask sends just the
	// changed fields and the TUI pane has no cap control — would otherwise leave a
	// positive cap on a cron/target-session task and have ValidateTrigger reject
	// the whole save. The rule lives here, in the shared merge, so the daemon,
	// TUI, API, and CLI all get it; an explicitly-patched cap is left alone so a
	// contradictory request still surfaces as an error instead of being silently
	// dropped.
	if u.MaxConcurrentRuns == nil {
		t.clearInapplicableCap()
	}
	return t
}

// capApplies reports whether this task's shape can carry a concurrency cap: it
// bounds sessions a watch task spawns per event, so it is meaningful only for a
// watch task that creates them (#1892). Cron fires already coalesce on RunTask's
// lock, and target-session deliveries already serialize into one session.
//
// The TargetSession test is trimmed to match deliverTaskPrompt's runtime
// "create a session per event" condition — the two must agree on what an empty
// target session is, or a cap could validate against one condition and be
// bypassed at delivery by the other. normalizeTargetSession is what keeps them
// in sync on the write path.
func (t Task) capApplies() bool {
	return t.IsWatch() && strings.TrimSpace(t.TargetSession) == ""
}

// clearInapplicableCap drops a stale positive cap from a task whose shape can no
// longer carry one.
func (t *Task) clearInapplicableCap() {
	if t.MaxConcurrentRuns > 0 && !t.capApplies() {
		t.MaxConcurrentRuns = 0
	}
}

// normalizeTargetSession trims the stored target session so validation and the
// runtime agree on what "no target session" means (#1892). ValidateTrigger and
// capApplies test TrimSpace(TargetSession) == "", but deliverTaskPrompt tests the
// RAW t.TargetSession == "": a whitespace-only value therefore validated as
// "creates a session per event" (so a cap was accepted and stored) while delivery
// took the target-session path and never passed the cap to CreateSession, quietly
// ignoring it. Normalizing on the write path collapses the two conditions onto one
// value rather than leaving two call-sites to agree forever. Whitespace-only was
// never a usable session title anyway — it now means the same thing as empty:
// create a session per run.
func (t *Task) normalizeTargetSession() {
	t.TargetSession = strings.TrimSpace(t.TargetSession)
}

// TaskEdit pairs a task ID with the field-level patch to apply to it. The TUI's
// task pane emits one per edited task (see DiffTask) so a save sends only the
// fields the user actually changed.
type TaskEdit struct {
	ID     string
	Update TaskUpdate
}

// DiffTask returns a TaskUpdate holding exactly the user-editable fields that
// differ between old and cur. The TUI uses it to turn an in-place edit of a
// cached task into a minimal patch, so saving one field never rewrites another
// that changed out-of-band while the editor was open (#1700/#1213).
func DiffTask(old, cur Task) TaskUpdate {
	var u TaskUpdate
	if cur.Name != old.Name {
		u.Name = &cur.Name
	}
	if cur.Prompt != old.Prompt {
		u.Prompt = &cur.Prompt
	}
	if cur.CronExpr != old.CronExpr {
		u.CronExpr = &cur.CronExpr
	}
	if cur.WatchCmd != old.WatchCmd {
		u.WatchCmd = &cur.WatchCmd
	}
	if cur.TargetSession != old.TargetSession {
		u.TargetSession = &cur.TargetSession
	}
	if cur.MaxConcurrentRuns != old.MaxConcurrentRuns {
		u.MaxConcurrentRuns = &cur.MaxConcurrentRuns
	}
	if cur.ProjectPath != old.ProjectPath {
		u.ProjectPath = &cur.ProjectPath
	}
	if cur.Program != old.Program {
		u.Program = &cur.Program
	}
	if cur.Enabled != old.Enabled {
		u.Enabled = &cur.Enabled
	}
	return u
}

// UpdateTask applies a field-level patch to the task with the given id under the
// file lock and returns the merged record. Only the patch's non-nil fields are
// written; every other field — including a value a concurrent writer committed
// after the caller read its copy — is preserved from the freshly-loaded record.
// This closes the full-struct read-modify-write clobber (#1700): an enable/
// disable toggle patches only Enabled and cannot revert another client's edit to
// the prompt, trigger, target session, or program.
//
// The merged task is validated (ValidateTrigger, plus the program enum when the
// patch sets Program) before it is written, so a patch that would leave the task
// in an invalid state is rejected. Scheduler-owned fields (LastRunAt/
// LastRunStatus) and CreatedAt are never patchable — UpdateTaskStatus remains
// their canonical writer (#731/#1215). Returns the not-found error when no task
// with the given id exists.
func UpdateTask(id string, update TaskUpdate) (Task, error) {
	if err := ValidateTaskID(id); err != nil {
		return Task{}, err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, err
	}
	var merged Task
	lockErr := config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}

		found := false
		for i, existing := range tasks {
			if existing.ID == id {
				merged = update.apply(existing)
				if err := merged.ValidateTrigger(); err != nil {
					return err
				}
				// Validate the program ONLY when the patch sets it: a toggle or
				// an unrelated field edit must not fail on a pre-existing Program
				// value that would no longer pass current enum validation (the
				// same tolerance UpdateTaskStatus applies to legacy records).
				if update.Program != nil && merged.Program != "" {
					if err := config.ValidateProgramEnum("task program", "task program", merged.Program, ""); err != nil {
						return err
					}
				}
				tasks[i] = merged
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("task with id %q not found", id)
		}

		return saveTasks(tasks)
	})
	if lockErr != nil {
		return Task{}, lockErr
	}
	return merged, nil
}
