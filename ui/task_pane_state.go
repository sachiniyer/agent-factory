package ui

import (
	"reflect"

	"github.com/sachiniyer/agent-factory/task"
)

// This file holds the TaskPane's task-list, selection, dirty-tracking, and
// focus/mode state accessors — the non-rendering, non-key-handling surface the
// app layer drives. Split out of task_pane.go to keep that file under the
// file-length limit (#1145); the rendering and key-handling code stays there.

// deletedRankEntry pairs the CAS expectation of a pending deletion with the
// load-order rank captured for that specific occurrence. Stored as a slice so
// that two deletions of the same task ID each carry their own rank — an
// ID-keyed map would overwrite the first occurrence's rank with the second's
// (PRRT_kwDORdIFwM6i6kOR).
type deletedRankEntry struct {
	expect task.Task
	rank   int
}

// restoredEntry tracks a single restored-deletion occurrence. expect is the
// stable CAS record used as occurrence identity; display is the currently
// visible row value, updated in place on each retry refresh. Keying by expect
// (not display) means a concurrent rebind that changes a field never causes the
// second-retry dedup check to miss and insert a ghost row
// (PRRT_kwDORdIFwM6i6kOK).
type restoredEntry struct {
	expect  task.Task
	display task.Task
}

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
	// loadedRanks records the ordinal(s) of each record in the set the pane
	// loaded, i.e. disk order, keyed by ID. Each ID maps to the slice of
	// load-order indices for every occurrence with that ID (tasks.json permits
	// duplicate IDs in hand-edited stores). The restore path orders against these
	// to put a row back where it belongs — a LIVE slice index is too unstable
	// because any other pending deletion renumbers the rows after it.
	s.loadedRanks = make(map[string][]int, len(tasks))
	for i, t := range tasks {
		s.originals[t.ID] = t
		s.loadedRanks[t.ID] = append(s.loadedRanks[t.ID], i)
	}
	s.deleted = nil
	s.deletedDisplays = nil
	s.deletedRanks = nil
	s.restoredDeletes = nil
	s.deferredRestoreBaseline = nil
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
	// Editing a row cancels any deletion still queued against it. A restored row
	// is an ordinary editable row that is ALSO awaiting a delete retry, and the
	// save runs edits before deletions — so without this the user's edit is
	// written and the record it was written to is then removed. This is the
	// mirror of deleteSelectedTask, which drops the task from the update set for
	// the same reason.
	s.cancelQueuedDeletion(id)
	s.dirtyIDs[id] = true
	s.dirty = true
}

// cancelQueuedDeletion drops ALL pending deletions of id, along with the
// restore bookkeeping that would otherwise keep treating the row as a retry in
// flight. It also recomputes the pane-wide dirty flag so that canceling the
// last queued deletion (with no edits pending) leaves dirty false — preventing
// saveContentPaneState from running an unnecessary reload that could block a
// subsequent run trigger if the reload fails (PRRT_kwDORdIFwM6i0U5T).
//
// All same-ID deletions are removed, not just the first one: when tasks.json
// contains duplicate IDs and the user edits or toggles a restored occurrence,
// RemoveTask removes every occurrence sharing that ID — so leaving any same-ID
// retry in the queue would delete the task the user just edited on the next
// save (PRRT_kwDORdIFwM6i5gqa).
func (s *TaskPane) cancelQueuedDeletion(id string) {
	// Collect the expect records being removed so we can remove the parallel
	// deletedDisplays entries. Walk backwards so index removal does not shift
	// unvisited positions.
	for i := len(s.deleted) - 1; i >= 0; i-- {
		d := s.deleted[i]
		if d.ID != id {
			continue
		}
		s.deleted = append(s.deleted[:i], s.deleted[i+1:]...)
		// Remove the parallel deletedDisplays entry so a subsequent
		// deleteSelectedTask (after the user edits then re-deletes this
		// row) does not find a stale snapshot from the cancelled
		// deletion. GetDeletedDisplay matches by full expect record, so
		// the pre-edit display would be returned for the new deletion —
		// causing the restore to show the pre-edit draft instead of the
		// newly-edited content (PRRT_kwDORdIFwM6i3K61).
		for j, p := range s.deletedDisplays {
			if reflect.DeepEqual(p.expect, d) {
				s.deletedDisplays = append(s.deletedDisplays[:j], s.deletedDisplays[j+1:]...)
				break
			}
		}
	}
	delete(s.restoredDeletes, id)
	delete(s.deferredRestoreBaseline, id)
	s.dirty = len(s.dirtyIDs) > 0 || len(s.deleted) > 0
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
// that reload. Re-appending to s.tasks keeps the row visible and re-queueing in
// s.deleted retries the removal on the next save. The record still exists on
// disk (the removal did not commit), so retrying RemoveTask is not the
// already-deleted re-run ConsumeDeleted drains to avoid (fixes #763).
//
// restoredDeletes deduplicates repeated failures: the second retry failure must
// not append another visible copy of the same row, only re-queue the delete.
// restoredDeletes persists across ConsumeDeleted passes and is only cleared by
// SetTasks (a successful reload) or AcknowledgeDeletedRestored (a successful
// retry), so the dedupe key is live for the entire retry sequence.
func (s *TaskPane) RestoreFailedDelete(tsk task.Task) {
	s.restoreFailedDeleteImpl(tsk, tsk, tsk)
}

// RestoreFailedDeleteWithExpect is like RestoreFailedDelete but separates the
// record the pane displays (display) from the one it queues for the deletion
// retry (expect). The originals baseline is set to expect so that a subsequent
// D uses the loaded original's project binding as the CAS — not a draft that
// was never persisted. Use this for the fallback path where display may be a
// draft the user was editing before pressing D.
func (s *TaskPane) RestoreFailedDeleteWithExpect(display, expect task.Task) {
	s.restoreFailedDeleteImpl(display, expect, expect)
}

// RestoreFailedDeleteWithFresh is like RestoreFailedDeleteWithExpect but sets
// the originals baseline to fresh (the display) rather than expect. Use this
// when the freshly-loaded set contains an authoritative record — one confirmed
// present by the daemon's own file — so that the pane and originals baseline
// reflect the current state and subsequent user actions (edits, re-deletes) are
// authorised against durable data rather than a stale binding that the daemon
// would refuse (PRRT_kwDORdIFwM6i06TE).
func (s *TaskPane) RestoreFailedDeleteWithFresh(fresh, expect task.Task) {
	s.restoreFailedDeleteImpl(fresh, expect, fresh)
}

// restoreFailedDeleteImpl is the shared implementation for RestoreFailedDelete,
// RestoreFailedDeleteWithExpect, and RestoreFailedDeleteWithFresh. display is
// inserted into s.tasks; baseline is snapshotted into s.originals; expect is
// queued in s.deleted for the retry.
func (s *TaskPane) restoreFailedDeleteImpl(display, expect, baseline task.Task) {
	if s.restoredDeletes == nil {
		s.restoredDeletes = make(map[string][]restoredEntry)
	}
	if s.originals == nil {
		s.originals = make(map[string]task.Task)
	}
	// Apply any deferred originals baseline that was withheld because the row
	// was in edit mode when the fresh authoritative record arrived. Now that we
	// are processing another retry for this ID, check if editing has ended and
	// apply the deferred baseline so subsequent re-deletes or edits use durable
	// state rather than the stale binding the edit form was opened against
	// (PRRT_kwDORdIFwM6i4rlG).
	if deferredBaseline, hasDeferral := s.deferredRestoreBaseline[display.ID]; hasDeferral && !s.editing {
		s.originals[display.ID] = deferredBaseline
		delete(s.deferredRestoreBaseline, display.ID)
	}
	// Check whether THIS specific occurrence (keyed by expect, the stable CAS
	// record) has already been restored. Keying by expect rather than display
	// means a concurrent field change between retries (e.g. another client
	// rebinds the task) does not cause the second retry to miss the dedup check
	// and insert a ghost row (PRRT_kwDORdIFwM6i6kOK). Storing per-ID slices
	// (not a single entry per ID) allows two duplicate-ID deletions to each get
	// their own restored occurrence (PRRT_kwDORdIFwM6i4rk4).
	storedEntries := s.restoredDeletes[display.ID]
	alreadyRestored := false
	storedIdx := -1
	for i, e := range storedEntries {
		if reflect.DeepEqual(e.expect, expect) {
			alreadyRestored = true
			storedIdx = i
			break
		}
	}
	if !alreadyRestored {
		pos := s.restorePosition(display.ID, expect)
		// baseline is the record to store in originals — the copy subsequent
		// user actions (re-delete via D, edit via ConsumeDirty) will diff
		// against and use as the CAS expectation. The caller selects it:
		// - RestoreFailedDelete: baseline == display == expect (all same)
		// - RestoreFailedDeleteWithExpect: baseline == expect (loaded original,
		//   for drafts where display may carry a never-persisted ProjectPath)
		// - RestoreFailedDeleteWithFresh: baseline == display (fresh authoritative
		//   reload, so subsequent actions use the durable current state)
		s.originals[display.ID] = baseline
		s.tasks = append(s.tasks[:pos], append([]task.Task{display}, s.tasks[pos:]...)...)
		// Carry the cursor over an insertion at or above it, so it keeps
		// naming the same record. This is not cosmetic: the edit form submits
		// its buffers into s.tasks[s.selectedIdx] (task_pane_edit.go:147-155),
		// so a stale index writes the task under edit into its neighbour.
		// Only advance when the cursor was on a real (pre-insertion) row:
		// when the pane was empty before the restore (selectedIdx==0, no row),
		// incrementing would leave selectedIdx==1 against a slice of length 1.
		if pos <= s.selectedIdx && s.selectedIdx < len(s.tasks)-1 {
			s.selectedIdx++
		}
		// Record the expect-display pair for this occurrence. The expect field
		// is the stable occurrence identity used for dedup on subsequent
		// retries; the display field is the currently visible row value, updated
		// in place on refresh passes so the in-place-update branch below can
		// locate the exact row (PRRT_kwDORdIFwM6i06TU, PRRT_kwDORdIFwM6i6kOK).
		s.restoredDeletes[display.ID] = append(storedEntries, restoredEntry{expect: expect, display: display})
	} else {
		// Second or later retry failure for this specific occurrence (same
		// expect): a fresh record may have been supplied (e.g. another client
		// changed the task between retries). Update the visible row and originals
		// baseline in place without inserting a duplicate.
		//
		// Find the SPECIFIC restored row by matching the display value stored at
		// restore time, not the first ID match. For duplicate-ID stores, an
		// ID-only search would update the FIRST occurrence instead of the
		// restored one, corrupting the earlier duplicate's content.
		//
		// Skip the in-place refresh if the user is currently editing this row:
		// the form buffers still contain the pre-refresh values, so replacing
		// the row and its originals baseline would turn those stale buffer
		// values into a "patch" that silently overwrites the concurrent change
		// when the form is submitted (PRRT_kwDORdIFwM6i0U5R). In that case,
		// defer the baseline update for the next pass when editing has ended
		// (PRRT_kwDORdIFwM6i4rlG).
		storedDisplay := storedEntries[storedIdx].display
		for i, t := range s.tasks {
			if !reflect.DeepEqual(t, storedDisplay) {
				continue
			}
			if s.editing && s.selectedIdx == i {
				// Defer the originals update: the form is open against the
				// current baseline; applying it now would make the form's
				// current buffer values appear as a patch against a moved
				// baseline. The next restoreFailedDeleteImpl call applies
				// it once editing has ended.
				if s.deferredRestoreBaseline == nil {
					s.deferredRestoreBaseline = make(map[string]task.Task)
				}
				s.deferredRestoreBaseline[display.ID] = baseline
			} else {
				// Use baseline (not display) to update originals, matching
				// what the first-restore branch does. display may contain
				// draft values that were never persisted; setting originals
				// to display would make those draft fields appear persisted:
				// a later toggle omits them from ConsumeDirty, and a
				// re-delete uses an unsaved ProjectPath as its CAS
				// expectation (PRRT_kwDORdIFwM6i3K68).
				s.originals[display.ID] = baseline
				s.tasks[i] = display
				// Update the stored display so subsequent retries can locate
				// the newly-refreshed row (not the stale pre-refresh value).
				s.restoredDeletes[display.ID][storedIdx] = restoredEntry{expect: expect, display: display}
			}
			break
		}
	}
	s.deleted = append(s.deleted, expect)
	s.dirty = true
}

// restorePosition returns the slice index at which a restored row belongs, so
// the pane keeps the order of the set it loaded — the order the sidebar reloads
// in, and the order showTasksOverlay's index-based selection transfer assumes.
//
// It orders against the rank captured in deletedRanks at delete time for the
// specific expect occurrence rather than a live slice index (which is renumbered
// by every other deletion) or the ID-keyed loadedRanks entry (which is the
// FIRST occurrence's rank — incorrect when a later duplicate is deleted,
// PRRT_kwDORdIFwM6i06TN). Rows the pane loaded keep their loaded order;
// anything without a rank (created in the pane, not yet reloaded) sorts after
// them, which is where it already sits.
func (s *TaskPane) restorePosition(id string, expect task.Task) int {
	rank := -1
	for _, e := range s.deletedRanks {
		if reflect.DeepEqual(e.expect, expect) {
			rank = e.rank
			break
		}
	}
	if rank < 0 {
		// Fall back to the first-occurrence rank from loadedRanks when no
		// per-deletion rank was captured (e.g. an earlier code path that did
		// not go through deleteSelectedTask).
		if ranks, known := s.loadedRanks[id]; known && len(ranks) > 0 {
			rank = ranks[0]
		} else {
			return len(s.tasks)
		}
	}
	for i, t := range s.tasks {
		var other int
		var known bool
		if t.ID == id {
			// Surviving row shares the restored ID — use its own occurrence
			// rank, not the deleted occurrence's rank stored in deletedRanks.
			// Using the deleted rank for the survivor would compare the deleted
			// row's rank against itself, placing the restored row on the
			// wrong side of the duplicate (PRRT_kwDORdIFwM6i1oQb).
			other, known = s.survivingSameIDRank(id, i, rank)
		} else {
			// Use the surviving occurrence's loaded rank, skipping any
			// pending-deletion siblings for this ID. Using a deleted sibling's
			// rank would misplace the restored row relative to the survivor
			// (PRRT_kwDORdIFwM6i7stc).
			other, known = s.survivingDifferentIDRank(t.ID, i)
		}
		if !known || other > rank {
			return i
		}
	}
	return len(s.tasks)
}

// survivingSameIDRank returns the original load rank for a surviving row that
// shares the given ID with the row being restored, at position pos in s.tasks.
// It reports whether a rank is known. deletedRank is the rank of the specific
// occurrence being restored (used to skip that slot in loadedRanks).
//
// When duplicate-ID rows exist and one was deleted, the survivor must not be
// compared using the deleted occurrence's rank. Instead, count same-ID
// survivors before pos to find the occurrence index, then pick the
// corresponding rank from loadedRanks while skipping the deleted slot. This
// places each remaining duplicate in its own original position.
func (s *TaskPane) survivingSameIDRank(id string, pos int, deletedRank int) (rank int, known bool) {
	// Count how many surviving rows with the same ID appear before pos.
	priorSurvivors := 0
	for j := 0; j < pos; j++ {
		if s.tasks[j].ID == id {
			priorSurvivors++
		}
	}
	ranks, hasLR := s.loadedRanks[id]
	if !hasLR || len(ranks) == 0 {
		return 0, false
	}
	skippedDeleted := false
	// Walk the sorted loadedRanks list, skipping the deleted slot, and return
	// the rank at the priorSurvivors-th surviving position.
	occIdx := 0
	for _, r := range ranks {
		if !skippedDeleted && r == deletedRank {
			// Skip the deleted occurrence's rank; consume the flag so it is
			// only skipped once (defensive against duplicate rank values).
			skippedDeleted = true
			continue
		}
		if occIdx == priorSurvivors {
			return r, true
		}
		occIdx++
	}
	return 0, false
}

// survivingDifferentIDRank returns the loaded rank for the surviving row at pos
// whose ID differs from the row being restored. It skips the ranks of deleted
// siblings that are truly absent (not yet restored to s.tasks) so that a
// deleted sibling's rank is never used in place of the visible survivor's rank
// (PRRT_kwDORdIFwM6i7stc).
func (s *TaskPane) survivingDifferentIDRank(id string, pos int) (rank int, known bool) {
	// Count how many surviving rows with this ID appear before pos.
	priorSurvivors := 0
	for j := 0; j < pos; j++ {
		if s.tasks[j].ID == id {
			priorSurvivors++
		}
	}
	ranks, hasLR := s.loadedRanks[id]
	if !hasLR || len(ranks) == 0 {
		return 0, false
	}
	// Build a multiset of deleted ranks for this ID. A deletion is "truly
	// absent" (not in s.tasks) when its deletedRanks entry has no corresponding
	// restoredDeletes entry. Restored occurrences ARE visible in s.tasks and
	// must not be skipped: their slot in loadedRanks represents a surviving row.
	deletedCounts := map[int]int{}
	for _, e := range s.deletedRanks {
		if e.expect.ID == id {
			deletedCounts[e.rank]++
		}
	}
	// Subtract restored occurrences: each restoredDeletes entry has a
	// matching deletedRanks entry whose rank belongs to a visible row — do not
	// skip it.
	for _, re := range s.restoredDeletes[id] {
		for _, de := range s.deletedRanks {
			if de.expect.ID == id && reflect.DeepEqual(de.expect, re.expect) {
				if deletedCounts[de.rank] > 0 {
					deletedCounts[de.rank]--
				}
				break
			}
		}
	}
	// Walk the sorted loadedRanks list, skipping truly-absent deleted slots,
	// and return the rank at the priorSurvivors-th surviving position.
	occIdx := 0
	for _, r := range ranks {
		if cnt := deletedCounts[r]; cnt > 0 {
			deletedCounts[r]--
			continue
		}
		if occIdx == priorSurvivors {
			return r, true
		}
		occIdx++
	}
	return 0, false
}

// AcknowledgeDeletedRestored removes all previously-restored rows for id from
// s.tasks when their deletion retry succeeds. Without this, a restored row
// remains visible until SetTasks runs — which is skipped while failedEdit is
// true — so a task deleted from disk would stay in the pane for the remainder
// of the retry sequence.
//
// All rows with a matching ID are removed: tasks.json permits duplicate IDs in
// hand-edited stores and task.RemoveTask removes every matching row from disk,
// so leaving extra ghost rows behind after a successful retry is incorrect.
//
// The row-removal loop runs even when id was not tracked in restoredDeletes —
// this handles the explicit-re-delete case: if a restored row's user re-presses
// D (clearing restoredDeletes[id] in deleteSelectedTask) and that new deletion
// later commits, AcknowledgeDeletedRestored must still sweep any remaining
// duplicate rows from s.tasks (PRRT_kwDORdIFwM6i06Tg).
//
// If a removed row is the currently selected entry and the pane is in edit
// mode, edit mode is exited to prevent renderEditMode from evaluating
// s.tasks[s.selectedIdx] against a removed entry and panicking.
func (s *TaskPane) AcknowledgeDeletedRestored(id string) {
	// Clear restore bookkeeping only when this ID was actually restored; the
	// early-exit is removed so the cleanup loop below always runs even for
	// explicit re-deletes that cleared the marker.
	if len(s.restoredDeletes[id]) > 0 {
		delete(s.restoredDeletes, id)
		delete(s.deferredRestoreBaseline, id)
		// Remove all deletedRanks entries for this ID.
		j := 0
		for _, e := range s.deletedRanks {
			if e.expect.ID != id {
				s.deletedRanks[j] = e
				j++
			}
		}
		s.deletedRanks = s.deletedRanks[:j]
		// Remove all deletedDisplays entries for this ID (there may be more
		// than one when duplicate-ID rows were both deleted and both failed).
		// RemoveTask removes every matching row from disk, so all display
		// records for that ID are now stale.
		i := 0
		for _, p := range s.deletedDisplays {
			if p.expect.ID != id {
				s.deletedDisplays[i] = p
				i++
			}
		}
		s.deletedDisplays = s.deletedDisplays[:i]
	}
	// Remove all rows with the given ID (tasks.json allows duplicate IDs;
	// RemoveTask removes every matching row from disk, so we must do the same
	// in the pane). Iterate backwards so index removal does not shift
	// unvisited positions. The loop is a no-op for normal (non-duplicate-ID)
	// deletions because deleteSelectedTask already removed the row from s.tasks.
	for i := len(s.tasks) - 1; i >= 0; i-- {
		if s.tasks[i].ID != id {
			continue
		}
		s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
		// Exit edit mode if the selected row was just removed: the index
		// now refers to a different (or nonexistent) entry, and
		// renderEditMode would panic on an out-of-range access.
		if s.selectedIdx == i {
			s.editing = false
		} else if s.selectedIdx > i {
			// The removed row sat ABOVE the cursor, so every row after it
			// shifted up by one. Follow the task the cursor was on: clamping
			// alone only repairs an index that fell off the end, leaving an
			// in-range one addressing its neighbour, and the next x/D/r
			// would act on a record the user never selected.
			s.selectedIdx--
		}
	}
	// Clamp the selection so it stays within the (now shorter) slice.
	if s.selectedIdx >= len(s.tasks) {
		s.selectedIdx = len(s.tasks) - 1
	}
	// …and never below it: removing the last row leaves an empty slice,
	// where len(s.tasks)-1 is -1.
	if s.selectedIdx < 0 {
		s.selectedIdx = 0
	}
}

// ConsumeDeleted returns the tasks pending deletion and clears the pane's
// deletion queue so a subsequent save can't reprocess already-deleted tasks.
// Failed edits restored after ConsumeDirty keep the pane dirty until the final
// reload succeeds or a later save retries them. The deletion loop in
// saveContentPaneState removes task records as a side effect, so re-running it
// would call RemoveTask on records that no longer exist and log spurious errors
// (fixes #763). restoredDeletes is NOT cleared here: it persists across passes
// so RestoreFailedDelete's dedupe check fires on the second (and later) retry
// failure, preventing a second visible copy of the same row from being appended
// to s.tasks. restoredDeletes is cleared by SetTasks on a successful reload and
// by AcknowledgeDeletedRestored when a retry succeeds.
func (s *TaskPane) ConsumeDeleted() []task.Task {
	deleted := s.deleted
	s.deleted = nil
	s.dirty = len(s.dirtyIDs) > 0
	return deleted
}

// GetDeletedDisplay returns the display record captured at delete time for the
// given expect record, and whether one was recorded. This is the exact row the
// user selected, before deleteSelectedTask replaced it with originals[id] for
// CAS purposes. When tasks.json contains duplicate IDs, originals[id] is the
// LAST duplicate, so this display record preserves the SELECTED row's content
// for the restore path.
//
// The lookup is keyed by the full expect record (not just ID) so that two
// concurrent deletions of different occurrences of the same ID each return
// their own display — an ID-only lookup would return the same (last) display
// for both (PRRT_kwDORdIFwM6i2XFw). The slice persists until SetTasks
// (successful reload) or AcknowledgeDeletedRestored (successful retry) removes
// entries.
//
// Entries are consumed on match: when two deletions of the same ID share
// identical expect records (deleteSelectedTask replaces both with the same
// originals[id]), the first call returns and removes the first match so the
// second call returns the next occurrence — preventing both failed deletions
// from being given the same display record (PRRT_kwDORdIFwM6i3K64).
func (s *TaskPane) GetDeletedDisplay(expect task.Task) (task.Task, bool) {
	for i, p := range s.deletedDisplays {
		if reflect.DeepEqual(p.expect, expect) {
			s.deletedDisplays = append(s.deletedDisplays[:i], s.deletedDisplays[i+1:]...)
			return p.display, true
		}
	}
	return task.Task{}, false
}

// IsRestoredDelete reports whether the task with id has already been restored
// to the visible pane by a prior RestoreFailedDelete pass. Used by the fallback
// (failed-reload) restore path to avoid overwriting an already-authoritative
// restored row with the stale pre-delete snapshot from deletedDisplays
// (PRRT_kwDORdIFwM6i3wJ2).
func (s *TaskPane) IsRestoredDelete(id string) bool {
	return len(s.restoredDeletes[id]) > 0
}

// CountRestoredDeletes returns the number of occurrences of id that have
// already been restored to the visible pane by a prior RestoreFailedDelete
// pass. This allows the caller to distinguish per-occurrence restore state
// when tasks.json contains duplicate IDs: IsRestoredDelete returns true after
// the FIRST occurrence is restored, but a second concurrent deletion of the
// same ID must still insert its own row — it is not yet restored
// (PRRT_kwDORdIFwM6i5gqZ).
func (s *TaskPane) CountRestoredDeletes(id string) int {
	return len(s.restoredDeletes[id])
}

// RequeueFailedDelete re-queues a deletion for retry without touching the
// already-restored visible row or its originals baseline. Use this when the
// row was restored with authoritative data from a prior pass and a subsequent
// fallback pass (no authoritative reload) must not regress it to the stale
// pre-delete display snapshot (PRRT_kwDORdIFwM6i3wJ2).
func (s *TaskPane) RequeueFailedDelete(tsk task.Task) {
	s.deleted = append(s.deleted, tsk)
	s.dirty = true
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
