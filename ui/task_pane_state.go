package ui

import "github.com/sachiniyer/agent-factory/task"

// This file holds the TaskPane's task-list, selection, dirty-tracking, and
// focus/mode state accessors — the non-rendering, non-key-handling surface the
// app layer drives. Split out of task_pane.go to keep that file under the
// file-length limit (#1145); the rendering and key-handling code stays there.

// SetTasks sets the task data.
func (s *TaskPane) SetTasks(tasks []task.Task) {
	s.tasks = tasks
	s.dirty = false
	s.dirtyIDs = nil
	// Snapshot the loaded records so ConsumeDirty can diff an edit against the
	// copy the pane started from and emit a field-level patch (#1700). Task is a
	// value type (its only pointer field, LastRunAt, is scheduler-owned and never
	// diffed), so a by-value copy is a sufficient baseline.
	s.originals = make(map[string]task.Task, len(tasks))
	for _, t := range tasks {
		s.originals[t.ID] = t
	}
	s.deleted = nil
	s.deletedPositions = nil
	s.editing = false
	// A reload replaces the create-form buffers a pending create was captured
	// against, so a create left un-consumed by a failed save must be dropped —
	// otherwise the next keypress after reopen fires it against the wrong
	// (reloaded) buffers and duplicates the now-selected task (#1531). Only
	// pendingCreate is cleared here: pendingTrigger is deliberately left intact
	// because saveContentPaneState reloads via SetTasks mid-flush and a pending
	// run-now must survive that reload to resolve by task ID (#1474). The
	// overlay-close path clears pendingTrigger instead (SetFocus(false)).
	s.pendingCreate = false
	if len(s.tasks) == 0 {
		s.selectedIdx = 0
	} else if s.selectedIdx >= len(s.tasks) {
		s.selectedIdx = len(s.tasks) - 1
	}
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
//
// If the task was previously restored to s.tasks by RestoreFailedDelete (and
// so still has a pending entry in s.deleted), editing or toggling it cancels
// the deletion retry: the user's intent is now to keep and modify the row, not
// to remove it. saveContentPaneState processes edits before deletions, so
// without this removal the edit would persist and the retained deletion would
// immediately undo it.
func (s *TaskPane) markTaskDirty(id string) {
	if s.dirtyIDs == nil {
		s.dirtyIDs = make(map[string]bool)
	}
	s.dirtyIDs[id] = true
	s.dirty = true
	// Cancel any pending deletion retry for this task.
	for i := len(s.deleted) - 1; i >= 0; i-- {
		if s.deleted[i].ID == id {
			s.deleted = append(s.deleted[:i], s.deleted[i+1:]...)
		}
	}
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

// RestoreFailedDelete makes a consumed deletion retryable and visible when its
// daemon removal fails with a non-committed error. The task was already removed
// from s.tasks at delete time, so without this restore it would vanish from the
// pane while the disk reload that would re-show it (SetTasks) is gated on
// !failedEdit — and a concurrent failed edit leaves failedEdit true, skipping
// that reload. Re-inserting at the task's original position keeps the row visible
// and the pane order consistent with the sidebar's disk-order reload; re-queueing
// in s.deleted retries the removal on the next save. The record still exists on
// disk (the removal did not commit), so retrying RemoveTask is not the
// already-deleted re-run ConsumeDeleted drains to avoid (fixes #763).
func (s *TaskPane) RestoreFailedDelete(tsk task.Task) {
	// Re-insert at the position the task held when it was deleted so the pane
	// order matches the sidebar's disk-order view. showTasksOverlay transfers
	// the sidebar's selected index directly into the TaskPane, so a position
	// mismatch would cause subsequent actions to target the wrong task.
	insertAt := len(s.tasks) // default: append
	if pos, ok := s.deletedPositions[tsk.ID]; ok && pos <= len(s.tasks) {
		insertAt = pos
		delete(s.deletedPositions, tsk.ID)
	}
	s.tasks = append(s.tasks[:insertAt], append([]task.Task{tsk}, s.tasks[insertAt:]...)...)
	s.deleted = append(s.deleted, tsk)
	s.dirty = true
}

// AcknowledgeDeletedRestored removes the task with the given ID from s.tasks
// when a previously restored deletion finally succeeds. Without this, a task
// re-inserted by RestoreFailedDelete stays visible after its retry RemoveTask
// call commits — the failedEdit gate that suppresses SetTasks also suppresses
// the reload that would otherwise remove it.
func (s *TaskPane) AcknowledgeDeletedRestored(id string) {
	for i, t := range s.tasks {
		if t.ID == id {
			s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
			if s.selectedIdx >= len(s.tasks) && s.selectedIdx > 0 {
				s.selectedIdx--
			}
			return
		}
	}
}

// PruneRestoredAbsent removes tasks from s.tasks that were re-queued by
// RestoreFailedDelete (and so appear in s.deleted) but are absent from the
// authoritative reload. This prevents ghost rows when a task was deleted by
// another client between the pane load and our RemoveTask call: the daemon
// returns a non-committed "not found" so RestoreFailedDelete fires, but the
// subsequent LoadTasksForCurrentRepo confirms the record is gone from disk.
// When failedEdit suppresses SetTasks, calling this method prunes those rows
// so the pane does not persist entries the disk no longer holds.
func (s *TaskPane) PruneRestoredAbsent(loaded []task.Task) {
	present := make(map[string]bool, len(loaded))
	for _, t := range loaded {
		present[t.ID] = true
	}
	// Walk backwards so index removals don't invalidate the remaining positions.
	for i := len(s.deleted) - 1; i >= 0; i-- {
		queued := s.deleted[i]
		if !present[queued.ID] {
			s.AcknowledgeDeletedRestored(queued.ID)
			// Also remove from the retry queue so the impossible deletion is
			// not re-submitted on the next save (the authoritative reload
			// confirmed the record is gone from disk).
			s.deleted = append(s.deleted[:i], s.deleted[i+1:]...)
		}
	}
}

// ConsumeDeleted returns the tasks pending deletion and clears the pane's
// deletion state so a subsequent save can't reprocess already-deleted tasks.
// Failed edits restored after ConsumeDirty keep the pane dirty until the final
// reload succeeds or a later save retries them. The deletion loop in
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
