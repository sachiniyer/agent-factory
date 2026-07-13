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
		edits = append(edits, task.TaskEdit{ID: t.ID, Update: update})
	}
	s.dirtyIDs = nil
	return edits
}

// ConsumeDeleted returns the tasks pending deletion and clears the pane's
// dirty state so a subsequent save can't reprocess already-deleted tasks. The
// deletion loop in saveContentPaneState removes task records as a side
// effect, so re-running it would call RemoveTask on records that no longer
// exist and log spurious errors (fixes #763).
func (s *TaskPane) ConsumeDeleted() []task.Task {
	deleted := s.deleted
	s.deleted = nil
	s.dirty = false
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
