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
	s.deleted = nil
	s.editing = false
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

// ConsumeDirty returns the tasks the user actually edited since the pane was
// loaded and clears the per-task dirty set. saveContentPaneState persists only
// these — never the full task list — so saving an edit to one task can't write
// a stale pane copy of an UNMODIFIED task back over a change another writer
// (CLI/daemon) committed while the pane was open (#1213). Mirrors the
// per-task tracking that ConsumeDeleted already does for deletions. The
// pane-wide dirty flag is left to ConsumeDeleted to clear so a save that has
// both edits and deletions still processes both.
func (s *TaskPane) ConsumeDirty() []task.Task {
	if len(s.dirtyIDs) == 0 {
		s.dirtyIDs = nil
		return nil
	}
	var modified []task.Task
	for _, t := range s.tasks {
		if s.dirtyIDs[t.ID] {
			modified = append(modified, t)
		}
	}
	s.dirtyIDs = nil
	return modified
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
