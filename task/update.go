package task

// The field-level task patch (#1700) and the update path that applies it.
//
// Split out of task.go so the store's read/create side and its patch/merge side
// are separately readable: this file owns TaskUpdate, the merge rules that
// decide what a patch actually stores, and UpdateTask/UpdateTaskChecked.

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

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
	MaxConcurrentRuns *int `json:"max_concurrent_runs,omitempty"`
	// OnComplete patches the spawned-session lifecycle verb (#2595). A pointer for
	// the same reason as MaxConcurrentRuns: "keep" is a meaningful value to patch
	// BACK to, and a plain string could not tell "revert to keep" from "leave it
	// alone".
	OnComplete  *string `json:"on_complete,omitempty"`
	ProjectPath *string `json:"project_path,omitempty"`
	Program     *string `json:"program,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
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
		u.OnComplete == nil &&
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
	}
	if u.MaxConcurrentRuns != nil {
		t.MaxConcurrentRuns = *u.MaxConcurrentRuns
	}
	if u.OnComplete != nil {
		t.OnComplete = *u.OnComplete
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
	// Canonicalize the MERGED target, unconditionally — never gated on
	// u.TargetSession != nil (#1892). Gating it there left the original defect live
	// on the legacy path: a task written before this rule can hold a whitespace-only
	// target on disk, and a patch that touches only the cap never enters the branch
	// that would fix it. ValidateTrigger would then read the target as empty and
	// accept the cap, while delivery read the raw value, took the target-session
	// path, and never passed the cap to CreateSession — the silently-ignored cap
	// this PR exists to close, reachable by simply not mentioning the field. Every
	// write now canonicalizes what it is about to store, so the record cannot
	// persist a value the two sides would read differently.
	t.canonicalizeTargetSession()
	t.canonicalizeOnComplete()
	// Drop a cap the merged record can no longer carry, unless this patch set it
	// explicitly (#1892). A partial patch that only moves the trigger or the
	// delivery mode — which is every non-CLI writer, since DiffTask sends just the
	// changed fields and the TUI pane has no cap control — would otherwise leave a
	// positive cap on a cron/target-session task and have ValidateTrigger reject
	// the whole save. The rule lives here, in the shared merge, so the daemon,
	// TUI, API, and CLI all get it; an explicitly-patched cap is left alone so a
	// contradictory request still surfaces as an error instead of being silently
	// dropped. It runs AFTER the canonicalization above, which decides what
	// capApplies sees.
	if u.MaxConcurrentRuns == nil {
		t.clearInapplicableCap()
	}
	// Same rule, same reason, for the spawned-session lifecycle (#2595): adding a
	// target_session to a per-run task that declares on_complete would otherwise
	// merge into a record ValidateTrigger rejects, so ordinary retargeting would
	// fail from every surface that does not expose the field — and force a CLI user
	// to know to pass --on-complete keep alongside. An explicitly-patched verb is
	// left alone so a contradictory request still surfaces as an error.
	if u.OnComplete == nil {
		t.clearInapplicableOnComplete()
	}
	return t
}

// TaskEdit pairs a task ID with the field-level patch to apply to it. The TUI's
// task pane emits one per edited task (see DiffTask) so a save sends only the
// fields the user actually changed.
//
// Expect pins the project binding of the record the pane LOADED (not the
// edited copy — an edit may itself move the task), so the daemon refuses the
// patch if another client rebound the task while the pane was open (#3230).
type TaskEdit struct {
	ID     string
	Update TaskUpdate
	Expect ProjectExpectation
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
	// Included before any editor offers it (#2595). An unchanged field patches
	// nothing either way, so this is inert today — but a differ that silently
	// omits a user-editable field is the #1700 clobber waiting to be reintroduced
	// the moment a surface starts editing it.
	if cur.OnComplete != old.OnComplete {
		u.OnComplete = &cur.OnComplete
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
// UpdateTask applies a field-level patch. expect optionally asserts, inside the
// same locked operation, that the task is still bound to the project the caller
// authorized it against — see ProjectExpectation.
// UpdateTask applies a patch with no declared actor, auditing it as
// ActorUnknown — see AddTask for why the undeclared form exists.
func UpdateTask(id string, update TaskUpdate, expect ProjectExpectation) (Task, error) {
	return UpdateTaskChecked(id, update, expect, ActorUnknown, nil)
}

// UpdateTaskChecked is UpdateTask with a validator applied to the authoritative
// merged record immediately before commit. Alongside an error, the validator
// may return a nonempty resolved RepoID to persist on a legacy row; no other
// field can be changed after ordinary task validation. A validator error leaves
// the stored task unchanged. See AddTaskChecked for the callback constraints.
//
// actor names the surface the patch came from. The audit entry is derived from
// the difference between the stored record and the merged one, so it describes
// what was actually written rather than what the caller asked for (#3623).
func UpdateTaskChecked(id string, update TaskUpdate, expect ProjectExpectation, actor Actor, validate func(Task) (string, error)) (Task, error) {
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
	// Re-resolve the retained RepoID when the patch rebinds the task, and do it
	// BEFORE taking the lock: this shells out to git, and holding the tasks.json
	// lock across a subprocess is the hazard ProjectExpectation's doc calls out.
	rebindRepoID := ""
	if update.ProjectPath != nil {
		rebindRepoID = repoIDForPath(*update.ProjectPath)
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
				// Verify against the freshly loaded record, before the patch is
				// applied — the pre-patch ProjectPath is what the caller
				// authorized against.
				if err := expect.Verify(existing); err != nil {
					return err
				}
				merged = update.apply(existing)
				if update.ProjectPath != nil {
					merged.RepoID = rebindRepoID
				}
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
				if validate != nil {
					validatedRepoID, err := validate(merged)
					if err != nil {
						return err
					}
					if validatedRepoID != "" && validatedRepoID != merged.RepoID {
						merged.RepoID = validatedRepoID
						// The daemon resolved a legacy row's binding as a side effect of
						// someone else's patch, and that write is durable. Recorded here or
						// nowhere: changedFields covers only patchable fields, and once
						// RepoID is set the stable-binding loader — which writes this same
						// daemon-upgrade entry on the path it owns — skips the row forever.
						// A no-op patch that backfills would otherwise leave no trace at all
						// (#3623 review). Attributed to the daemon rather than to the caller,
						// because nobody asked for it.
						appendAudit(&merged, ActorDaemonUpgrade, AuditUpdated, []string{"repo_id"}, nowFn())
					}
				}
				// Diffed against the record just loaded under this lock, and
				// stamped before the write — so the trail records the change that
				// actually landed, at the instant it landed.
				auditUpdate(&existing, &merged, actor, nowFn())
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
