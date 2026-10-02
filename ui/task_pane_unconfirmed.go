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
		if !ok || !s.editLanded(draft, record) {
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

// editLanded reports whether a held draft's unconfirmed edit is now reflected
// in the reloaded record. The settlement must compare ONLY the fields the user
// actually patched, against their canonical forms — never the whole record by
// raw bytes.
//
// The prior whole-record test (task.DiffTask(record, draft).IsEmpty()) compared
// the daemon-canonicalized reloaded record against the pane's raw, non-canonical
// draft, so any daemon-side normalization defeated it even when the edit landed.
// Two normalizations run on every save (task.TaskUpdate.apply):
//
//   - canonicalizeTargetSession turns an all-whitespace target into "". A draft
//     holding "   " never matched a reload holding "".
//   - clearInapplicableCap zeroes MaxConcurrentRuns the moment a task gains a
//     target session. The pane has no cap control (the field is absent from the
//     edit form), so a retarget keeps the draft's old cap while the reload has
//     it cleared — and the cap was never in the user's patch to begin with.
//
// Field-scoping closes both. The patch is the diff against the pane's baseline
// (s.originals), so it carries exactly the fields the user moved; a daemon side
// effect on a field the user never touched (the cleared cap) is never compared.
// The fields the patch DOES carry are compared through their canonical forms, so
// a whitespace target, an "ARCHIVE" verb, or any other value the daemon stores
// canonicalized matches the reload. Fields the daemon stores verbatim are
// compared raw, since apply writes the patched pointer through unchanged.
//
// A patch that is empty (the user toggled and reverted, or the patch was never
// held) carries no work to confirm, so it lands by definition — there is nothing
// a lost reply could have left unsaved.
func (s *TaskPane) editLanded(draft, record task.Task) bool {
	patch := task.DiffTask(s.originals[draft.ID], draft)
	if patch.IsEmpty() {
		return true
	}
	if patch.Name != nil && *patch.Name != record.Name {
		return false
	}
	if patch.Prompt != nil && *patch.Prompt != record.Prompt {
		return false
	}
	if patch.CronExpr != nil && *patch.CronExpr != record.CronExpr {
		return false
	}
	if patch.WatchCmd != nil && *patch.WatchCmd != record.WatchCmd {
		return false
	}
	if patch.TargetSession != nil &&
		task.CanonicalTargetSession(*patch.TargetSession) != task.CanonicalTargetSession(record.TargetSession) {
		return false
	}
	if patch.MaxConcurrentRuns != nil && *patch.MaxConcurrentRuns != record.MaxConcurrentRuns {
		return false
	}
	if patch.OnComplete != nil &&
		task.CanonicalOnComplete(*patch.OnComplete) != task.CanonicalOnComplete(record.OnComplete) {
		return false
	}
	if patch.ProjectPath != nil && *patch.ProjectPath != record.ProjectPath {
		return false
	}
	if patch.Program != nil && *patch.Program != record.Program {
		return false
	}
	if patch.Enabled != nil && *patch.Enabled != record.Enabled {
		return false
	}
	return true
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
