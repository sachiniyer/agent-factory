package ui

import (
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/task"
)

// This file holds the TaskPane's handling of an edit whose save could not be
// confirmed (#4824): the daemon may have applied it with only the reply lost.
// Two contracts meet here. A draft is never dropped without a positive
// not-found (#4798), so the edit stays in the pane. And an uncertain mutation is
// never re-sent automatically (#4820), so the next save skips it until the user
// edits the task again, which is an explicit re-save.

// HoldUnconfirmedEdit keeps a consumed edit whose save could not be confirmed.
// It stays dirty and visible across reloads, like RestoreFailedEdit's, but
// ConsumeDirty does not return it: only a new edit to the task sends it again.
func (s *TaskPane) HoldUnconfirmedEdit(id string) {
	s.markTaskDirty(id)
	if s.unconfirmedIDs == nil {
		s.unconfirmedIDs = make(map[string]bool)
	}
	s.unconfirmedIDs[id] = true
	s.unconfirmedQuitWarned = false
}

// keepUnconfirmedDirty is the dirty set ConsumeDirty leaves behind: the held
// unconfirmed edits it skipped, or nil when there are none.
func (s *TaskPane) keepUnconfirmedDirty() map[string]bool {
	var kept map[string]bool
	for id := range s.unconfirmedIDs {
		if !s.dirtyIDs[id] {
			continue
		}
		if kept == nil {
			kept = make(map[string]bool, len(s.unconfirmedIDs))
		}
		kept[id] = true
	}
	return kept
}

// settleConfirmedDrafts runs at the top of SetTasks. A held unconfirmed edit
// whose values the loaded record now carries did land: it is settled as clean,
// takes the loaded record, and is named for TakeSettledDraftNotice so it never
// changes state silently. One the load does not match stays held.
func (s *TaskPane) settleConfirmedDrafts(loaded []task.Task) {
	if len(s.unconfirmedIDs) == 0 {
		return
	}
	formID := ""
	if s.editing && s.selectedTaskInRange() {
		formID = s.tasks[s.selectedIdx].ID
	}
	byID := make(map[string]task.Task, len(loaded))
	for _, t := range loaded {
		if _, seen := byID[t.ID]; !seen {
			byID[t.ID] = t
		}
	}
	for _, draft := range s.tasks {
		if !s.unconfirmedIDs[draft.ID] || draft.ID == formID {
			continue
		}
		record, ok := byID[draft.ID]
		if !ok || !task.DiffTask(record, draft).IsEmpty() {
			continue
		}
		delete(s.unconfirmedIDs, draft.ID)
		delete(s.dirtyIDs, draft.ID)
		name := draft.Name
		if name == "" {
			name = draft.ID
		}
		s.settledDrafts = append(s.settledDrafts, name)
	}
	s.dirty = len(s.dirtyIDs) > 0 || len(s.deleted) > 0
}

// TakeSettledDraftNotice returns one notice naming every unconfirmed edit a
// reload has since shown landed, and clears them; "" when there is none.
func (s *TaskPane) TakeSettledDraftNotice() string {
	names := s.settledDrafts
	s.settledDrafts = nil
	if len(names) == 0 {
		return ""
	}
	return "Saved edits to " + quoteNames(names) + " — the task list now shows them"
}

// TakeUnconfirmedQuitNotice returns, once per held set, a notice that quitting
// leaves the held unconfirmed edits unsaved; "" when there are none or the
// user was already told. The first quit stops to show it and the next goes.
func (s *TaskPane) TakeUnconfirmedQuitNotice() string {
	if s.unconfirmedQuitWarned {
		return ""
	}
	var names []string
	for _, t := range s.tasks {
		if s.unconfirmedIDs[t.ID] && s.dirtyIDs[t.ID] {
			name := t.Name
			if name == "" {
				name = t.ID
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	s.unconfirmedQuitWarned = true
	return "Edits to " + quoteNames(names) + " could not be confirmed and were not re-sent — check the task list; quit again to leave them unsaved"
}

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ")
}
