package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the #4798 follow-up: when DiscardDeletedDraft drops
// the very row an open edit form is bound to, the form must close so its stale
// field buffers are neither rendered over a shifted/emptied list nor written
// into the neighbour the cursor re-clamped onto. The single-task case used to
// panic renderEditMode at `tsk := s.tasks[s.selectedIdx]` (index out of range)
// and tear the TUI down; the two-task case used to silently re-target the form
// onto a neighbour and clobber it on the next Enter. The contract is the one
// DeleteTask (#4838) already enforces for its row-removal path: a removed row
// must not leave a stale form open over a shifted selection. The render and
// Enter-save guards in task_pane_edit.go defend the same indexing for any
// future row-removal path that reaches the form orphaned anyway; the two
// guard tests here pin those guards so they cannot be silently removed.

// formOnFirst sets the pane up like the tasks overlay does on open (#1249):
// tasks loaded, focus taken, and the edit form dropped straight onto index 0.
// The first task is a cron task with a real git repo path so a save's
// validateForm would pass, which lets the guard tests reach the indexed write
// (and prove the guard, not validation, stops it).
func formOnFirst(t *testing.T, tasks ...task.Task) *TaskPane {
	t.Helper()
	s := NewTaskPane()
	s.SetTasks(tasks)
	s.SetFocus(true)
	s.EnterEditSelected()
	require.True(t, s.IsEditing(), "precondition: the edit form is open")
	require.True(t, s.selectedTaskInRange(), "precondition: the cursor is on the form-bound row")
	return s
}

// toggleDirtyFromForm marks the form-bound task dirty the way the user does:
// from inside the form at the trigger-type selector stop (#4838 made the list
// verbs reachable there), `x` routes to toggleSelectedTask and toggles the row
// without closing the form — exactly the in-form dirty state the save-on-quit
// path catches mid-edit.
func toggleDirtyFromForm(t *testing.T, s *TaskPane) {
	t.Helper()
	// The form opens focused on Name (a text stop). Tab once to the trigger
	// selector — a non-text stop where the verb glyph is reachable, not typed.
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}))
	require.False(t, s.IsTextFieldFocused(), "Tab lands on the trigger selector, a non-text stop")
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.IsDirty(), "precondition: the form-bound task is dirty without the form closing")
	require.True(t, s.IsEditing(), "precondition: the form is still open while dirty")
}

// Single task, form open on it, discard it: the form must close and the next
// render must not panic. Before the fix the list emptied under an open form and
// the title lookup `tsk := s.tasks[s.selectedIdx]` panicked (index out of range
// [0] with length 0), killing the TUI.
func TestDiscardDeletedDraftClosesFormBoundToOnlyTask(t *testing.T) {
	only := cronEditTask(t)
	only.ID = "only-1"
	s := formOnFirst(t, only)
	toggleDirtyFromForm(t, s)

	s.DiscardDeletedDraft("only-1")

	require.False(t, s.IsEditing(), "the form must close — its row is gone")
	require.False(t, s.IsCreating())
	assert.Empty(t, s.GetTasks(), "the discarded row goes")
	assert.False(t, s.IsDirty(), "the discard cleared the dirty edit")
	// Capture the rendered string inside NotPanics so a regression (form left
	// open over an empty list, render panics) fails the test cleanly instead of
	// cascading through a second unguarded String() call.
	var rendered string
	assert.NotPanics(t, func() { rendered = s.String() }, "rendering after the discard must not panic")
	assert.NotContains(t, rendered, "Edit task", "the orphaned form title is gone with the form")
	assert.NotEmpty(t, s.TakeDiscardedDraftNotice(), "the discard still raises its notice")
}

// Two tasks, form open on alpha, discard alpha: the form must close and bravo
// must keep its own values. Before the fix the cursor silently re-clamped onto
// bravo with the form still open and alpha's buffers still in it, so the next
// Enter wrote alpha's name/prompt/cron/etc. into bravo and queued a field-level
// patch for the wrong task — the silent cross-task clobber the #1700/#1249-era
// field-level diff exists to prevent.
func TestDiscardDeletedDraftClosesFormBoundToAlphaLeavesBravoUntouched(t *testing.T) {
	alpha := cronEditTask(t)
	alpha.ID, alpha.Name, alpha.Prompt = "alpha-1", "alpha", "alpha prompt"
	bravo := cronEditTask(t)
	bravo.ID, bravo.Name, bravo.Prompt = "bravo-1", "bravo", "bravo prompt"
	s := formOnFirst(t, alpha, bravo)
	// Snapshot bravo exactly as the pane loaded it: any post-discard change to
	// bravo is a clobber.
	bravoBefore := s.GetTasks()[1]
	toggleDirtyFromForm(t, s) // marks alpha dirty, form stays open on alpha

	s.DiscardDeletedDraft("alpha-1")

	require.False(t, s.IsEditing(), "the form must close — its row (alpha) is gone")
	assert.Equal(t, []string{"bravo-1"}, paneIDs(s), "only alpha is dropped; bravo stays")
	bravoAfter := s.GetTasks()[0]
	assert.Equal(t, bravoBefore, bravoAfter, "bravo's row is byte-identical — no stale-buffer write")
	assert.Equal(t, "bravo", bravoAfter.Name)
	assert.Equal(t, "bravo prompt", bravoAfter.Prompt)
	assert.NotEmpty(t, s.TakeDiscardedDraftNotice())
}

// The render guard keeps a future row-removal path (one that forgets to close
// the form, unlike DiscardDeletedDraft) from panicking renderEditMode when the
// list empties under an open form. This test constructs that orphaned state by
// hand: validateForm is bypassed by never submitting; String() simply must not
// panic.
func TestRenderEditModeDoesNotPanicWhenSelectionIsOutOfRange(t *testing.T) {
	only := cronEditTask(t)
	only.ID = "only-2"
	s := formOnFirst(t, only)
	require.True(t, s.selectedTaskInRange())

	// Simulate a rogue row-removal that leaves the form open over an empty
	// list — the exact state the render guard defends.
	s.tasks = nil
	s.selectedIdx = 0
	require.False(t, s.selectedTaskInRange())

	// Capture inside NotPanics so a missing guard fails cleanly instead of a
	// second unguarded String() call cascading the panic past the test.
	var out string
	assert.NotPanics(t, func() { out = s.String() }, "the render guard must skip the title lookup")
	// The form body still renders (the fields are buffer-backed); only the
	// orphaned "Edit task <id>" title is omitted. The body is enough to prove
	// the guard skipped the indexed access rather than crashing before it.
	assert.NotContains(t, out, "Edit task only-2", "the orphaned title is not rendered")
}

// The Enter-save guard keeps a future row-removal path that leaves the form open
// over an empty list from panicking OR writing into a non-existent row when the
// user presses Enter. The form is built from a valid task (validateForm would
// pass), then the list is emptied under it; Enter must consume without writing
// and without panicking.
func TestEditSaveGuardRefusesWriteWhenSelectionIsOutOfRange(t *testing.T) {
	only := cronEditTask(t)
	only.ID, only.Name = "only-3", "name-from-form"
	s := formOnFirst(t, only)
	// Pre-seed a different name into the buffer to prove the guard stops the
	// write (rather than the buffer happening to match the row).
	typeIntoPane(s, "-edited")
	require.Contains(t, s.editName.Value(), "-edited")

	// Empty the list under the open form — the state the save guard defends.
	s.tasks = nil
	s.selectedIdx = 0
	require.False(t, s.selectedTaskInRange())

	assert.NotPanics(t, func() {
		// Enter on the prompt field would insert a newline; land on the Save
		// button instead so the submit path runs.
		s.focusIndex = taskFocusSave
		consumed := s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
		require.True(t, consumed, "Enter is consumed by the form")
	}, "the save guard must return before the indexed write")
	require.False(t, s.selectedTaskInRange())
	assert.True(t, s.IsEditing(), "the guard consumes Enter without closing the form; nothing was saved")
	assert.Empty(t, s.ConsumeDirty(), "no field-level patch is queued for the gone row")
}

// The Enter-save guard does not change the documented save path when the
// selection IS in range — a plain Enter on the Save button of a valid, in-range
// edit form still writes the buffers into the row and closes the form.
func TestEditSaveGuardKeepsInrangeSavePath(t *testing.T) {
	only := cronEditTask(t)
	only.ID = "only-4"
	s := formOnFirst(t, only)
	typeIntoPane(s, " renamed")
	// Land on Save and submit it.
	s.focusIndex = taskFocusSave
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))

	assert.False(t, s.IsEditing(), "Enter saved and closed the form")
	require.Len(t, s.GetTasks(), 1)
	assert.True(t, strings.HasSuffix(s.GetTasks()[0].Name, "renamed"), "the rewrite landed on the in-range row")
	dirty := s.ConsumeDirty()
	require.Len(t, dirty, 1)
	assert.Equal(t, "only-4", dirty[0].ID, "the patch targets the edited task")
}
