package ui

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/task"
)

// This file holds the TaskPane's task-list, selection, dirty-tracking, and
// focus/mode state accessors — the non-rendering, non-key-handling surface the
// app layer drives. Split out of task_pane.go to keep that file under the
// file-length limit (#1145); the rendering and key-handling code stays there.

// SetTasks reconciles the pane with a freshly loaded task list, so a reload can
// run at any time without costing the user their work (#4487). A row the user
// has not touched takes the loaded record. What the user is holding survives:
//
//   - A row with an unsaved or failed edit (dirtyIDs) keeps its edited values
//     and the baseline its patch is diffed against. Rebasing it would put
//     another writer's fields into the patch (#1700) and pin a project binding
//     the user never authorized (#3230).
//   - The row an open edit form is bound to is held the same way, because Enter
//     writes the form into that row and diffs it against that baseline.
//   - A row queued for deletion stays hidden, and the queue is kept.
//
// A held row the load no longer contains stays visible, after the loaded rows,
// so a draft is never dropped silently: the save reports why it failed, and
// deleting the row discards it (that save then also reports the task as not
// found). Loaded rows keep disk order ahead of it, so an index taken from the
// rail still names the same task. The cursor follows its task by ID.
//
// Only the user's values are held. A held row also keeps the run status and
// schedule health it was loaded with until it saves, as the whole pane did
// before; the rail shows the fresh ones.
//
// SetTasks used to discard all of this, so callers had to skip it while an
// edit was live, and the pane then hid anything the skipped reload would have
// shown — a task whose deletion failed vanished (#4257), and restoring it by
// hand instead appended a copy on every further failure (#4488).
//
// It reports whether the rows or the selection changed. A list from a
// different scope, which nothing held belongs to, goes through ResetTasks.
func (s *TaskPane) SetTasks(tasks []task.Task) bool {
	held := s.heldRows()
	queued := make(map[string]bool, len(s.deleted))
	for _, d := range s.deleted {
		queued[d.ID] = true
	}
	selectedID := ""
	if s.selectedTaskInRange() {
		selectedID = s.tasks[s.selectedIdx].ID
	}

	next := make([]task.Task, 0, len(tasks)+len(held))
	// Snapshot the loaded records so ConsumeDirty can diff an edit against the
	// copy the pane started from and emit a field-level patch (#1700). Task is a
	// value type (its only pointer field, LastRunAt, is scheduler-owned and never
	// diffed), so a by-value copy is a sufficient baseline.
	originals := make(map[string]task.Task, len(tasks)+len(held))
	placed := make(map[string]bool, len(held))
	for _, t := range tasks {
		if queued[t.ID] {
			continue
		}
		if draft, ok := held[t.ID]; ok {
			// Placed once even if a damaged file lists the ID twice: a second
			// copy of a draft is a second row the user could edit or delete.
			if !placed[t.ID] {
				placed[t.ID] = true
				next = append(next, draft)
			}
			continue
		}
		next = append(next, t)
		originals[t.ID] = t
	}
	for _, t := range s.tasks {
		if _, ok := held[t.ID]; ok && !placed[t.ID] {
			placed[t.ID] = true
			next = append(next, held[t.ID])
		}
	}
	for id := range held {
		if original, ok := s.originals[id]; ok {
			originals[id] = original
		}
	}

	changed := !sameTasks(s.tasks, next)
	s.tasks = next
	s.originals = originals
	s.dirty = len(s.dirtyIDs) > 0 || len(s.deleted) > 0
	// A reload replaces the create-form buffers a pending create was captured
	// against, so a create left un-consumed by a failed save must be dropped —
	// otherwise the next keypress after reopen fires it against the wrong
	// (reloaded) buffers and duplicates the now-selected task (#1531). Only
	// pendingCreate is cleared here: pendingTrigger is deliberately left intact
	// because saveContentPaneState reloads via SetTasks mid-flush and a pending
	// run-now must survive that reload to resolve by task ID (#1474). The
	// overlay-close path clears pendingTrigger instead (SetFocus(false)).
	s.pendingCreate = false

	previousIdx := s.selectedIdx
	s.selectTaskID(selectedID)
	return changed || s.selectedIdx != previousIdx
}

// selectTaskID moves the cursor to the task with the given ID. When that task
// is gone the cursor keeps its position, clamped, so it lands on a neighbour.
func (s *TaskPane) selectTaskID(id string) {
	if id != "" {
		for i, t := range s.tasks {
			if t.ID == id {
				s.selectedIdx = i
				return
			}
		}
	}
	if len(s.tasks) == 0 || s.selectedIdx < 0 {
		s.selectedIdx = 0
	} else if s.selectedIdx >= len(s.tasks) {
		s.selectedIdx = len(s.tasks) - 1
	}
}

// ResetTasks replaces the pane's list and discards everything the user was
// holding against the old one: edits, pending deletions, and an open edit
// form. It is for a list from a different scope — a project switch — where a
// held row would belong to another project. A reload of the same list goes
// through SetTasks.
func (s *TaskPane) ResetTasks(tasks []task.Task) {
	s.dirtyIDs = nil
	s.deleted = nil
	s.editing = false
	s.tasks = nil
	s.SetTasks(tasks)
}

// heldRows returns the rows a reload must not replace, keyed by ID: every row
// with an unsaved edit, and the row an open edit form is bound to.
//
// A damaged file can list an ID twice, and the reload keeps one copy of a held
// ID, so it must be the copy carrying the user's work: the form's row, else the
// first copy that differs from its baseline.
func (s *TaskPane) heldRows() map[string]task.Task {
	held := make(map[string]task.Task, len(s.dirtyIDs)+1)
	formID := ""
	if s.editing && s.selectedTaskInRange() {
		formID = s.tasks[s.selectedIdx].ID
		held[formID] = s.tasks[s.selectedIdx]
	}
	for _, t := range s.tasks {
		if !s.dirtyIDs[t.ID] || t.ID == formID {
			continue
		}
		if kept, seen := held[t.ID]; !seen || (s.unedited(kept) && !s.unedited(t)) {
			held[t.ID] = t
		}
	}
	return held
}

// unedited reports whether t carries no change a save would send.
func (s *TaskPane) unedited(t task.Task) bool {
	return task.DiffTask(s.originals[t.ID], t).IsEmpty()
}

func sameTasks(a, b []task.Task) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// GetTasks returns the current tasks.
func (s *TaskPane) GetTasks() []task.Task {
	return s.tasks
}

// SelectTask moves the list selection to idx (clamped). The tasks overlay
// uses it to open the manager on the task the in-rail cursor was resting on.
func (s *TaskPane) SelectTask(idx int) {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s.tasks) {
		idx = len(s.tasks) - 1
	}
	if idx >= 0 {
		s.selectedIdx = idx
	}
}

// markTaskDirty records that the task with the given ID was edited so a later
// save persists it. It also sets the pane-wide dirty flag that gates whether
// saveContentPaneState runs at all (#1213).
func (s *TaskPane) markTaskDirty(id string) {
	if s.dirtyIDs == nil {
		s.dirtyIDs = make(map[string]bool)
	}
	s.dirtyIDs[id] = true
	s.dirty = true
}

// ConsumeDirty returns a field-level patch for each task the user actually
// edited since the pane was loaded and clears the per-task dirty set. Each edit
// carries ONLY the fields that differ from the copy the pane loaded (diffed via
// task.DiffTask against the originals snapshot), so saving an edit to one field
// can't write a stale pane copy of an UNCHANGED field back over a change another
// writer (CLI/daemon) committed while the pane was open — the #1700 clobber (of
// which #1213's per-task tracking was only a partial fix). An edit whose diff is
// empty (a value toggled and reverted) is dropped: there is nothing to persist.
// Mirrors the per-task tracking ConsumeDeleted does for deletions; the pane-wide
// dirty flag is left to ConsumeDeleted to clear so a save with both edits and
// deletions still processes both.
func (s *TaskPane) ConsumeDirty() []task.TaskEdit {
	if len(s.dirtyIDs) == 0 {
		s.dirtyIDs = nil
		return nil
	}
	var edits []task.TaskEdit
	for _, t := range s.tasks {
		if !s.dirtyIDs[t.ID] {
			continue
		}
		update := task.DiffTask(s.originals[t.ID], t)
		if update.IsEmpty() {
			continue
		}
		// Pin the project binding of the ORIGINAL record — the copy the user's
		// edit was authorized against — never the edited task, whose
		// ProjectPath may itself be the change (#3230). The daemon re-verifies
		// it under its file lock, so a task rebound by another client while
		// the pane was open is refused instead of patched.
		edits = append(edits, task.TaskEdit{
			ID:     t.ID,
			Update: update,
			Expect: task.ExpectProject(s.originals[t.ID]),
		})
	}
	s.dirtyIDs = nil
	return edits
}

// AcknowledgeSavedEdit advances one task's diff baseline after its daemon
// update succeeds. The update contains every user-editable field that differs
// from the previous baseline, so the pane's current value is exactly the new
// baseline for later edits even if the caller's final disk reload fails.
func (s *TaskPane) AcknowledgeSavedEdit(id string) {
	for _, current := range s.tasks {
		if current.ID != id {
			continue
		}
		if s.originals == nil {
			s.originals = make(map[string]task.Task)
		}
		s.originals[id] = current
		delete(s.dirtyIDs, id)
		return
	}
}

// RestoreFailedEdit makes a consumed edit retryable when its daemon update
// fails. Its in-memory values and patch remain dirty through background reloads
// so retry cannot discard the only copy of the user's changes.
func (s *TaskPane) RestoreFailedEdit(id string) {
	s.markTaskDirty(id)
}

// DiscardDeletedDraft drops the unsaved edit to a task a save has just proved
// deleted — the daemon answered the update with "not found" (#4798). Keeping it
// would retry that save, and report the same failure, on every close forever.
// The row goes with it, and the task's name is queued for
// TakeDiscardedDraftNotice so the draft is never dropped silently. A row with
// nothing a save would send is dropped without a notice: no work was lost.
//
// Only a positive not-found may call this. Absence from a reload is not one —
// the reload can be scoped to another project, or simply fail — so SetTasks
// keeps a draft whose task it cannot see, and any other save failure goes
// through RestoreFailedEdit.
func (s *TaskPane) DiscardDeletedDraft(id string) {
	kept := s.tasks[:0:0]
	lostWork := false
	for _, t := range s.tasks {
		if t.ID != id {
			kept = append(kept, t)
			continue
		}
		if !s.unedited(t) {
			lostWork = true
		}
	}
	if len(kept) == len(s.tasks) {
		return
	}
	if lostWork {
		// The name the task was loaded with, not a rename the draft carries:
		// it is the name the user last saw on the rail and in `af tasks list`.
		name := s.originals[id].Name
		if name == "" {
			name = id
		}
		s.discardedDrafts = append(s.discardedDrafts, name)
	}
	selectedID := ""
	if s.selectedTaskInRange() {
		selectedID = s.tasks[s.selectedIdx].ID
	}
	s.tasks = kept
	delete(s.dirtyIDs, id)
	delete(s.originals, id)
	s.dirty = len(s.dirtyIDs) > 0 || len(s.deleted) > 0
	s.selectTaskID(selectedID)
}

// TakeDiscardedDraftNotice returns one notice naming every draft
// DiscardDeletedDraft dropped since the last call, and clears them. It returns
// "" when nothing was dropped. The save that drops a draft usually runs as the
// overlay closes, so the app raises this on its own notice bar, not the pane.
func (s *TaskPane) TakeDiscardedDraftNotice() string {
	names := s.discardedDrafts
	s.discardedDrafts = nil
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	if len(quoted) == 1 {
		return "Discarded unsaved edits to " + quoted[0] + " — the task was deleted"
	}
	return "Discarded unsaved edits to " + strings.Join(quoted, ", ") + " — the tasks were deleted"
}

// ConsumeDeleted returns the tasks pending deletion and clears the pane's
// deletion state so a subsequent save can't reprocess already-deleted tasks.
// Failed edits restored after ConsumeDirty keep the pane dirty, across reloads,
// until a later save lands them. The deletion loop in
// saveContentPaneState removes task records as a side effect, so re-running it
// would call RemoveTask on records that no longer exist and log spurious errors
// (fixes #763).
func (s *TaskPane) ConsumeDeleted() []task.Task {
	deleted := s.deleted
	s.deleted = nil
	s.dirty = len(s.dirtyIDs) > 0
	return deleted
}

// IsDirty returns true if tasks were modified.
func (s *TaskPane) IsDirty() bool {
	return s.dirty
}

// HasFocus returns whether the pane has input focus.
func (s *TaskPane) HasFocus() bool {
	return s.hasFocus
}

// SetFocus sets the focus state.
func (s *TaskPane) SetFocus(focus bool) {
	s.hasFocus = focus
	if !focus {
		s.editing = false
		s.creating = false
		// Closing the overlay (Esc drops focus) must also drop any pending
		// create/trigger whose save failed and left it un-consumed: otherwise
		// it survives the close and fires on the next keypress after reopen,
		// against the reloaded buffers, duplicating a task (#1531).
		s.pendingCreate = false
		s.pendingTrigger = false
		s.pendingTriggerID = ""
	}
}

// IsEditing returns true if in edit mode.
func (s *TaskPane) IsEditing() bool {
	return s.editing
}

// IsCreating returns true if in create mode.
func (s *TaskPane) IsCreating() bool {
	return s.creating
}
