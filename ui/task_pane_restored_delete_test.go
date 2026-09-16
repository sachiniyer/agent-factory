package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
)

// Invariants the restored-row bookkeeping (RestoreFailedDelete /
// AcknowledgeDeletedRestored) must hold for the rest of the pane. The restore
// path re-inserts a row the pane had already removed, so it has to leave the
// slice, the deletion queue and the selection in the shape every other pane
// operation assumes — position-addressed rendering and index-addressed
// selection transfer from the rail both read those.

// restorePaneWithFailedDelete builds a three-task pane, deletes the FIRST task,
// and restores it the way a non-committed delete failure does. First is
// deliberate: a restore that appends puts the row somewhere it did not start,
// which is what the ordering and selection invariants below are about.
func restorePaneWithFailedDelete(t *testing.T) (*TaskPane, task.Task) {
	t.Helper()
	repo := newGitRepo(t)
	mk := func(id, name string) task.Task {
		return task.Task{
			ID: id, Name: name, Prompt: "p", CronExpr: "* * * * *",
			ProjectPath: repo, Program: "claude", Enabled: true,
		}
	}
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{mk("a", "alpha"), mk("b", "bravo"), mk("c", "charlie")})
	tp.SetFocus(true)

	tp.SelectTask(0)
	tp.deleteSelectedTask()
	queued := tp.ConsumeDeleted()
	require.Len(t, queued, 1)
	require.Equal(t, "a", queued[0].ID)

	// The delete did not commit, so the pane restores the row for retry.
	tp.RestoreFailedDelete(queued[0])
	return tp, queued[0]
}

// TestRestoreFailedDeleteKeepsDiskOrder: the restored row must come back where
// it was, not at the end.
//
// The sidebar reloads from disk in disk order while the pane keeps its own
// slice (SetTasks is gated on !failedEdit), and showTasksOverlay transfers the
// rail's selection into the pane BY INDEX. Append reorders the pane relative to
// the rail, so the transferred index then addresses a different record and a
// following toggle/delete/run acts on the wrong task.
func TestRestoreFailedDeleteKeepsDiskOrder(t *testing.T) {
	tp, _ := restorePaneWithFailedDelete(t)

	ids := []string{}
	for _, tsk := range tp.GetTasks() {
		ids = append(ids, tsk.ID)
	}
	assert.Equal(t, []string{"a", "b", "c"}, ids,
		"a restored row must return to its original position; appending it desyncs the pane from the rail's index")
}

// TestRestoreFailedDeleteDoesNotDoubleQueueOnReDelete: pressing D again on a
// restored row must not queue a second deletion of the same record.
//
// RestoreFailedDelete re-queues the delete for retry AND makes the row visible,
// so the row the user sees is already queued. A second queue entry makes the
// save call RemoveTask twice: the first commits, the second answers "not found",
// and the user gets a save failure for a deletion that actually succeeded.
func TestRestoreFailedDeleteDoesNotDoubleQueueOnReDelete(t *testing.T) {
	tp, restored := restorePaneWithFailedDelete(t)

	idx := -1
	for i, tsk := range tp.GetTasks() {
		if tsk.ID == restored.ID {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0, "the restored row must be visible")
	tp.SelectTask(idx)
	tp.deleteSelectedTask()

	queued := tp.ConsumeDeleted()
	ids := []string{}
	for _, tsk := range queued {
		ids = append(ids, tsk.ID)
	}
	assert.Equal(t, []string{"a"}, ids,
		"re-deleting an already-queued restored row must not add a second entry")
}

// TestAcknowledgeDeletedRestoredLeavesSelectionRenderable: removing the
// restored row must leave the pane renderable.
//
// AcknowledgeDeletedRestored splices s.tasks while the pane may be in edit mode
// on that very row, and SetTasks — which would reset editing and clamp the
// selection — is skipped whenever a concurrent edit failed. renderEditMode
// indexes s.tasks[s.selectedIdx] unguarded (ui/task_pane_edit.go:348), so a
// stale selection past the end panics on the next render, after the user
// dismisses the recovery notice.
func TestAcknowledgeDeletedRestoredLeavesSelectionRenderable(t *testing.T) {
	tp, restored := restorePaneWithFailedDelete(t)
	tp.SetSize(80, 24)

	idx := -1
	for i, tsk := range tp.GetTasks() {
		if tsk.ID == restored.ID {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	tp.SelectTask(idx)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing(), "the restored row is open in the editor")

	// The deletion retry commits: the row must go, but the pane stays in
	// whatever mode it was in because SetTasks is skipped on a failed edit.
	tp.AcknowledgeDeletedRestored(restored.ID)

	assert.NotPanics(t, func() { _ = tp.String() },
		"rendering after the restored row is removed must not panic on a stale selection")
	if tp.IsEditing() {
		assert.Less(t, tp.selectedIdx, len(tp.GetTasks()),
			"if the pane is still editing, the selection must address a real row")
	}
}

// TestAcknowledgeDeletedRestoredKeepsCursorOnSameTask: removing the restored
// row must leave the cursor on the task it was on, not on that task's neighbour.
//
// Sibling of the ordering finding, reached through the removal path instead of
// the insert path. Clamping alone only repairs a selection that fell off the
// end; when the removed row sat ABOVE the cursor, every later row shifts up by
// one and an unclamped index silently addresses the next task along. The next
// `x`, `D` or `r` then acts on a record the user never selected — the same
// wrong-row hazard, one step removed.
func TestAcknowledgeDeletedRestoredKeepsCursorOnSameTask(t *testing.T) {
	repo := newGitRepo(t)
	mk := func(id string) task.Task {
		return task.Task{
			ID: id, Name: id, Prompt: "p", CronExpr: "* * * * *",
			ProjectPath: repo, Program: "claude", Enabled: true,
		}
	}
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{mk("a"), mk("b"), mk("c"), mk("d")})
	tp.SetFocus(true)

	// Delete the FIRST row and have that delete fail, so the restored row lands
	// above the cursor.
	tp.SelectTask(0)
	tp.deleteSelectedTask()
	queued := tp.ConsumeDeleted()
	require.Len(t, queued, 1)
	tp.RestoreFailedDelete(queued[0])
	require.Equal(t, "a", tp.GetTasks()[0].ID, "the restored row sits above the cursor")

	// Park the cursor on "c" and confirm it is really there.
	for i, tsk := range tp.GetTasks() {
		if tsk.ID == "c" {
			tp.SelectTask(i)
		}
	}
	sel, ok := tp.SelectedTask()
	require.True(t, ok)
	require.Equal(t, "c", sel.ID)

	// The deletion retry commits and the row above the cursor goes away.
	tp.AcknowledgeDeletedRestored("a")

	sel, ok = tp.SelectedTask()
	require.True(t, ok, "a task must still be selected")
	assert.Equal(t, "c", sel.ID,
		"the cursor must follow its task when a row above it is removed, not slide onto the next one")
}
