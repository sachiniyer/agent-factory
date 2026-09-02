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
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// OnComplete is what happens to a session this task SPAWNED once its run
	// finishes (#2595). Empty — the default — means OnCompleteKeep, so every task
	// written before this field existed behaves exactly as it always has.
	//
	// The problem it solves is that a per-run session had no lifecycle at all: a
	// daily cron task produced one session per fire, each finished its work, went
	// idle, and then held a tmux session and a git worktree forever. The only
	// thing standing between a schedule and unbounded growth was prose in the
	// prompt ("finally, run af sessions archive --self"), which nothing enforces
	// and which `af tasks list` cannot show. This moves the decision onto the
	// task, where it is declared once and visible.
	//
	// Both non-keep verbs are offered because the choice between them is the
	// actual decision, not an implementation detail: archive keeps the session
	// restorable but retains its whole worktree — gitignored build output
	// included — indefinitely, while kill reclaims that disk and prunes the
	// session's owned branch. A run whose output already landed in a pushed
	// branch or a PR usually wants kill; a run that might need inspecting wants
	// archive. af must not pick for the user, so the default does neither.
	OnComplete  string `json:"on_complete,omitempty"`
	ProjectPath string `json:"project_path"`
	// RepoID is the owning project's repo ID, resolved ONCE at bind time and
	// retained. It is derived, never patchable — AddTask sets it and UpdateTask
	// recomputes it when ProjectPath changes.
	//
	// It exists because config.RepoIDFromRoot is sha256(main-repo-root): the ID
	// is a pure function of a PATH, so it can only be computed while that path
	// still resolves. A task may record a subdirectory or linked worktree of its
	// project (the TUI stores the path the user typed); delete that directory
	// and re-deriving the ID is impossible — the path no longer resolves to any
	// repo, and hashing the stale leaf invents an ID that matches nothing. This
	// field snapshots the resolution performed while the path WAS valid, so a
	// vanished subdirectory can never strand a task outside its own project.
	//
	// Empty on rows written before this field existed; scope matching falls back
	// to resolving ProjectPath for those (see api/scope.go). Not backfilled by a
	// migration on purpose: the backfill would have to shell out to git for
	// every task inside the tasks.json load path, and rows pick the field up as
	// they are next written anyway.
	RepoID        string     `json:"repo_id,omitempty"`
	Program       string     `json:"program"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
	// Audit is the bounded trail of mutations to this task — the one field here
	// that is a HISTORY rather than a current value, and the only way to answer
	// "did someone turn this off?" (#3623). Written by the store inside the same
	// locked operation that commits the change; see task/audit.go.
	Audit []AuditEntry `json:"audit,omitempty"`

	// The four fields below are DERIVED AT READ TIME and never persisted — see
	// task/overdue.go for the derivation and saveTasks for the strip that
	// enforces it. They are ordinary fields on the record rather than a side
	// channel because every surface that reads a task needs them, and a health
	// signal that lives somewhere other than the thing it describes is a signal
	// people forget to look at.

	// Overdue reports that this task's last run precedes its most recent
	// scheduled occurrence by more than one slack window.
	Overdue bool `json:"overdue,omitempty"`
	// MissedOccurrences counts the fires the schedule has had since that last
	// run, saturating at MaxMissedOccurrences.
	MissedOccurrences int `json:"missed_occurrences,omitempty"`
	// NextRunAt is what the LIVE scheduler will actually fire next, read off its
	// armed entry rather than recomputed from the expression. Absent when the
	// task is not armed, and that absence is itself the signal.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// Arming is the live arming observation: ArmingArmed, ArmingNotArmed, or
	// ArmingUnknown (the zero value) when no daemon answered.
	Arming string `json:"arming,omitempty"`
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
		if CanonicalTargetSession(t.TargetSession) != "" {
			return fmt.Errorf("task %s sets both max_concurrent_runs and target_session; deliveries into one session already serialize, so drop the cap or drop the target session", t.ID)
		}
	}
	// on_complete governs a session this task SPAWNED, so it is meaningful only
	// where the task spawns one (#2595). Rejecting it elsewhere follows the cap's
	// precedent above for the same reason: a setting that reads as enforced but
	// silently does nothing is the gotcha class this repo designs away.
	switch onComplete := t.SessionLifecycle(); onComplete {
	case OnCompleteKeep:
	case OnCompleteArchive, OnCompleteKill:
		if CanonicalTargetSession(t.TargetSession) != "" {
			return fmt.Errorf("task %s sets both on_complete=%s and target_session; a target session is one long-lived session you named for reuse, and %sing it after every run would destroy the thing the target exists to reuse — drop on_complete, or drop the target session to get a session per run", t.ID, onComplete, onComplete)
		}
	default:
		return fmt.Errorf("task %s has an unknown on_complete %q; use one of %s", t.ID, t.OnComplete, strings.Join(OnCompleteValues(), ", "))
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

// MigrateOnLoadPath returns the absolute path of tasks.json, which LoadTasks
// rewrites in place when it migrates a legacy file to the current schema at daemon
// load (task/schema_migration.go -> config.LoadAndMigrateSchemaFile). The upgrade
// transaction manifest (#2212 R3) snapshots it so a binary-only rollback restores
// tasks in a schema the previous daemon can read.
func MigrateOnLoadPath() (string, error) {
	return getTasksPathFn()
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

	tasks, err := tasksFromSchemaBytes(data)
	if err != nil {
		return nil, err
	}
	// Every READ carries the schedule-health derivation (#3623). Deriving it here
	// rather than in each surface is what stops the next surface from being the
	// one that renders a dark task as healthy. loadTasksLocked — the WRITE path's
	// loader — deliberately does not, so nothing derived can reach disk.
	return WithScheduleHealth(tasks, nowFn()), nil
}

// nowFn is the clock the read-time derivation reads. A package variable so tests
// can pin it; production reads the wall clock.
var nowFn = time.Now

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
//
// It strips the read-time derived fields on the way out (#3623). Every load now
// populates them, so a load-modify-save path would otherwise store a stale
// "overdue" — a claim about an instant that has already passed by the time
// anything reads it back. Enforcing it in the ONE writer rather than in each
// caller is what makes "derived, never persisted" a property instead of a
// convention.
func saveTasks(tasks []Task) error {
	path, err := getTasksPathFn()
	if err != nil {
		return err
	}
	stored := make([]Task, len(tasks))
	copy(stored, tasks)
	for i := range stored {
		stored[i].stripDerived()
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal tasks: %w", err)
	}
	enveloped, err := marshalTasksEnvelope(data)
	if err != nil {
		return fmt.Errorf("failed to marshal tasks envelope: %w", err)
	}
	return config.AtomicWriteFile(path, enveloped, 0644)
}

// AddTask appends a task with no declared actor, recording the create in the
// audit trail as ActorUnknown. Production writers go through the daemon, which
// calls AddTaskChecked with the surface the request came from; this wrapper
// exists for callers that genuinely have no surface to name.
func AddTask(t Task) error {
	_, err := AddTaskChecked(t, ActorUnknown, nil)
	return err
}

// AddTaskChecked is AddTask with a final pre-commit validator. The validator
// runs after RepoID derivation and trigger validation, under the tasks-file
// lock, so an error guarantees that no task was appended. It must not shell out
// or recursively access the task store. The daemon uses this to make target-
// session lifecycle validation atomic with archive fencing (#2646). On success
// it returns the canonical record that was appended, including derived fields
// such as RepoID, so callers do not publish a stale request projection.
//
// actor names the surface the create came from; it is recorded as this task's
// first audit entry (#3623).
func AddTaskChecked(t Task, actor Actor, validate func(Task) error) (Task, error) {
	if err := ValidateTaskID(t.ID); err != nil {
		return Task{}, err
	}
	// Canonicalize before validating so validation judges exactly what will be
	// stored — a whitespace-only target session must not validate as "no target
	// session" and then behave as one at delivery time (#1892).
	t.canonicalizeTargetSession()
	t.canonicalizeOnComplete()
	if err := t.ValidateTrigger(); err != nil {
		return Task{}, err
	}
	// Empty Program means "fall back to the configured default_program at
	// run time"; only validate when an explicit per-task override was set.
	if t.Program != "" {
		if err := config.ValidateProgramEnum("task program", "task program", t.Program, ""); err != nil {
			return Task{}, err
		}
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, err
	}
	// Resolve the owning project's ID now, while ProjectPath is known to
	// resolve, and retain it — see Task.RepoID. Outside the lock: this shells
	// out to git. A path that does not resolve leaves RepoID empty rather than
	// failing the add, so callers that bind a task to a not-yet-existing path
	// keep working; scope matching falls back to resolving the path for those.
	t.RepoID = repoIDForPath(t.ProjectPath)
	lockErr := config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}
		if validate != nil {
			if err := validate(t); err != nil {
				return err
			}
		}
		// Stamped inside the lock, immediately before the append, so the audit
		// timestamp is the commit's and an entry can never exist for a create
		// that a validator or a failed write rolled back.
		appendAudit(&t, actor, AuditCreated, nil, nowFn())
		tasks = append(tasks, t)
		return saveTasks(tasks)
	})
	if lockErr != nil {
		return Task{}, lockErr
	}
	return t, nil
}

// repoIDForPath resolves a project path to its owning repo's canonical ID,
// returning "" when the path does not resolve to a repository. It never falls
// back to hashing the path: an ID that no repo actually has is worse than no ID
// at all, because a caller cannot tell the two apart.
func repoIDForPath(projectPath string) string {
	if strings.TrimSpace(projectPath) == "" {
		return ""
	}
	repo, err := config.RepoFromPath(projectPath)
	if err != nil {
		return ""
	}
	return repo.ID
}

// RemoveTask deletes a task. expect optionally asserts, inside the same locked
// operation, that the task is still bound to the project the caller authorized
// it against — see ProjectExpectation.
func RemoveTask(id string, expect ProjectExpectation) error {
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
				// Verify against the record just loaded under the lock, not one
				// the caller carried in — that freshness is the whole point.
				if err := expect.Verify(t); err != nil {
					return err
				}
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

// LoadTasksForCurrentRepo returns only the tasks belonging to the git
// repository containing cwd.
func LoadTasksForCurrentRepo() ([]Task, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	return LoadTasksForRepo(repo.Root)
}

// LoadTasksForRepo returns only the tasks belonging to the project rooted at
// repoRoot, which must be a main-worktree root as returned by
// config.CurrentRepo / config.RepoFromPath. It is the repo-scoped loader the
// TUI task pane and the in-place project switch (#1461) use to populate the
// automations strip.
//
// Membership is decided by repo IDENTITY, not path strings: a task bound to a
// SUBDIRECTORY or a linked worktree of repoRoot belongs to it, because that is
// what git says (#2098). See repo_scope.go.
func LoadTasksForRepo(repoRoot string) ([]Task, error) {
	return loadTasksForScope(newRepoScope(repoRoot))
}

func loadTasksForScope(scope *repoScope) ([]Task, error) {
	all, err := LoadTasks()
	if err != nil {
		return nil, err
	}
	var filtered []Task
	for _, t := range all {
		matched, _ := scope.matches(t)
		if matched {
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

// capApplies reports whether this task's shape can carry a concurrency cap: it
// bounds sessions a watch task spawns per event, so it is meaningful only for a
// watch task that creates them (#1892). Cron fires already coalesce on RunTask's
// lock, and target-session deliveries already serialize into one session.
//
// The TargetSession test goes through CanonicalTargetSession, the same function
// deliverTaskPrompt's runtime "create a session per event" condition uses — the
// two must agree on what an empty target session is, or a cap could validate
// against one condition and be bypassed at delivery by the other.
func (t Task) capApplies() bool {
	return t.IsWatch() && CanonicalTargetSession(t.TargetSession) == ""
}

// clearInapplicableCap drops a stale positive cap from a task whose shape can no
// longer carry one.
func (t *Task) clearInapplicableCap() {
	if t.MaxConcurrentRuns > 0 && !t.capApplies() {
		t.MaxConcurrentRuns = 0
	}
}

// onCompleteApplies reports whether this task's shape can carry a spawned-session
// lifecycle: it governs a session the task CREATED, so it is meaningful only when
// the task creates one. A target-session task delivers into a session the user
// named for reuse, which is not the task's to reap.
func (t Task) onCompleteApplies() bool {
	return CanonicalTargetSession(t.TargetSession) == ""
}

// clearInapplicableOnComplete drops a stale lifecycle verb from a task whose
// shape can no longer carry one.
func (t *Task) clearInapplicableOnComplete() {
	if t.SessionLifecycle() != OnCompleteKeep && !t.onCompleteApplies() {
		t.OnComplete = ""
	}
}

// CanonicalTargetSession is THE canonical form of a target session title, and the
// single answer to "does this task have a target session?" (#1892). Every side
// must ask it — the write path, ValidateTrigger, capApplies, and
// deliverTaskPrompt — because a cap is accepted based on this question and then
// enforced based on it again. Two call-sites answering it differently is exactly
// the defect this function exists to remove: validation once read
// TrimSpace(TargetSession) == "" while delivery read the RAW TargetSession == "",
// so a whitespace-only target validated as "creates a session per event" (cap
// accepted and stored) and then took the target-session path at delivery, which
// never passes the cap to CreateSession. The cap was silently ignored.
//
// The rule:
//
//   - An ALL-WHITESPACE value means no target session. It was never a usable
//     title — validateTitleAvailableLocked rejects TrimSpace(title) == "" on
//     every creation path — so it can only ever have been a typo for empty.
//
//   - Any other value is returned BYTE-IDENTICAL. It is NOT trimmed, and this is
//     the load-bearing half. A session title may legally contain leading or
//     trailing spaces: title validation rejects only all-whitespace, and the
//     daemon keys its instances on the exact title bytes. Trimming a nonempty
//     target would therefore silently REDIRECT the task to a different session —
//     a task aimed at " build " would look up "build", miss it, and then fail on
//     auto-create, because " build " and "build" sanitize to the same git branch
//     and collide. Titles are not canonicalized globally, so the stored target
//     must stay byte-identical to what delivery looks up.
func CanonicalTargetSession(target string) string {
	if strings.TrimSpace(target) == "" {
		return ""
	}
	return target
}

// canonicalizeTargetSession puts the stored target into canonical form. Applied
// on every write, unconditionally — see apply.
func (t *Task) canonicalizeTargetSession() {
	t.TargetSession = CanonicalTargetSession(t.TargetSession)
}
