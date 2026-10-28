package app

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the repro for the discard-edit/form-orphan bug introduced by
// #4798 (commit ba7a2102): DiscardDeletedDraft removed a save-proved-deleted
// row but never closed the open edit form bound to it, so:
//   - With a single task, the next render panicked at
//     `tsk := s.tasks[s.selectedIdx]` (index out of range [0] with length 0)
//     and Bubbletea tore the whole TUI down with ErrProgramKilled /
//     ErrProgramPanic. The form was still open at that render because
//     ctrl+c routed to handleQuit before the form's own ctrl+c-cancel branch
//     could run (#1727), and handleQuit aborted the quit to show the
//     discarded-draft notice — leaving the user in the overlay with the form
//     open over an empty list.
//   - With two tasks, the cursor silently re-clamped onto the neighbour and
//     the form continued rendering/saving with the discarded task's stale
//     field buffers, so pressing Enter wrote them into the neighbour and
//     queued a field-level patch the next save committed — silent cross-task
//     data corruption, the clobber class #1700/#1249-era fixes prevent.
//
// Every path that removes a row the edit form is bound to must close the form,
// the contract DeleteTask (#4838) already enforces; the fix mirrors it in
// DiscardDeletedDraft, with render/Enter guards in task_pane_edit.go defending
// the same indexing for any future row-removal path. The render/Enter guards
// alone would prevent the crash but leave the silent overwrite reachable, so
// the form-close is the load-bearing fix — these app-level tests are its
// end-to-end pin through the real ctrl+c → handleQuit → saveContentPaneState →
// DiscardDeletedDraft path the bug report walks. The UI-level tests in
// ui/task_pane_discard_edit_form_test.go pin the render/Enter guards and the
// re-seed-on-reopen directly.

// homeWithFormOpenOnDeletedTask sets up a home whose task pane holds real
// on-disk tasks, with the edit form dropped open on the first task the way
// the tasks overlay does on open (#1249's one-step flow), marked dirty from
// inside the form by an `x` toggle at the trigger selector — the in-form dirty
// state the save-on-quit catches mid-edit. The caller simulates the
// out-of-band deletion and installs the not-found stub before driving ctrl+c.
func homeWithFormOpenOnDeletedTask(t *testing.T, tasks ...task.Task) (*home, *ui.TaskPane, task.Task) {
	t.Helper()
	require.NotEmpty(t, tasks, "need at least one task to open the edit form on")
	h := newTestHome(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	for i := range tasks {
		tasks[i].ProjectPath = repo.Root
		require.NoError(t, task.AddTask(tasks[i]))
	}
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)

	sp := h.automations.TaskPane()
	sp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	sp.SetFocus(true)
	sp.EnterEditSelected() // #1249's one-step overlay-open: form open on index 0.
	require.True(t, sp.IsEditing(), "precondition: the edit form is open on the first task")
	primary, ok := sp.SelectedTask()
	require.True(t, ok, "precondition: the form is bound to a row")
	require.Equal(t, loaded[0].ID, primary.ID, "precondition: the form is bound to the first task")

	// From inside the form, Tab to the trigger-type selector (a non-text stop
	// where the list verbs are reachable per #4838) and press `x` to toggle
	// the form-bound task dirty. The form stays open and the row is now dirty
	// — exactly the in-form dirty state the bug's save-on-quit catches.
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}))
	require.False(t, sp.IsTextFieldFocused(), "Tab lands on the trigger selector, a non-text stop")
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	require.True(t, sp.IsDirty(), "precondition: the form-bound task is dirty")
	require.True(t, sp.IsEditing(), "precondition: the form is still open while dirty")

	h.state = stateTasks
	return h, sp, loaded[0]
}

// Single task, form open on it, ctrl+c quits: the daemon positively proves the
// task was deleted (another writer removed it out-of-band), DiscardDeletedDraft
// drops the row, and the fix closes the form. The quit is aborted to show the
// discarded-draft notice, and the user is left in the overlay with the form
// CLOSED over the now-empty list — the next render returns the "No tasks"
// view instead of panicking at the title lookup.
//
// Before the fix, the form stayed open after DiscardDeletedDraft (the only
// state it touched was tasks/dirtyIDs/originals); selectTaskID clamped
// selectedIdx to 0 over `len(s.tasks) == 0`; the next renderEditMode panicked
// on `s.tasks[0]` and killed the TUI.
func TestReproDiscardEditCtrlC_ClosesFormOnOnlyTask_NoPanic(t *testing.T) {
	alpha := task.Task{
		ID: "alpha-1", Name: "alpha", Prompt: "alpha prompt",
		CronExpr: "0 9 * * *", Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	h, sp, alpha := homeWithFormOpenOnDeletedTask(t, alpha)

	// Another writer deleted alpha out-of-band while the pane still held it.
	require.NoError(t, task.RemoveTask(alpha.ID, task.ProjectExpectation{}))
	t.Cleanup(SetTaskUpdaterForTest(func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
		return fmt.Errorf("task with id %q not found", id)
	}))

	// ctrl+c is routed unconditionally to handleQuit (#1727): the form's own
	// ctrl+c-cancel branch never runs for this keypress from the tasks overlay.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyCtrlC})

	require.False(t, h.quitting, "the discard notice aborts the quit to display itself")
	require.Equal(t, stateTasks, h.state, "the user stays in the tasks overlay")
	require.False(t, sp.IsEditing(), "the form must close — its row is gone")
	require.False(t, sp.IsCreating())
	assert.NotPanics(t, func() { _ = sp.String() }, "the next render must not panic")
	assert.Empty(t, sp.GetTasks(), "the discarded task's row is gone")
	assert.NotContains(t, sp.String(), "Edit task", "no orphaned edit-form title is rendered")
	text, isFailure := h.errBox.RetainedNotice()
	assert.False(t, isFailure, "a discarded draft is a notice, not a failure")
	assert.Contains(t, text, `Discarded unsaved edits to "alpha"`)
	// The next save must not retry — alpha is dropped, not retained.
	assert.Empty(t, sp.ConsumeDirty(), "no patch is queued for the gone task")
}

// Two tasks, form open on alpha, ctrl+c quits: the daemon returns not-found
// for alpha, DiscardDeletedDraft drops alpha and the fix closes the form.
// Bravo (the surviving neighbour) keeps its own values — no stale-buffer write
// into the wrong row. Before the fix the cursor silently re-clamped onto
// bravo with the form still open and alpha's buffers still in it, so the next
// Enter wrote alpha's name/prompt/cron/etc. into bravo and queued a
// field-level patch the next save committed — silent cross-task clobber.
func TestReproDiscardEditCtrlC_ClosesFormOnAlpha_BravoUntouched(t *testing.T) {
	alpha := task.Task{
		ID: "alpha-1", Name: "alpha", Prompt: "alpha prompt",
		CronExpr: "0 9 * * *", Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	bravo := task.Task{
		ID: "bravo-1", Name: "bravo", Prompt: "bravo prompt",
		CronExpr: "0 10 * * *", Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	h, sp, alpha := homeWithFormOpenOnDeletedTask(t, alpha, bravo)
	bravoBefore := sp.GetTasks()[1]

	// Another writer deleted alpha out-of-band while the pane holds alpha+bravo.
	require.NoError(t, task.RemoveTask(alpha.ID, task.ProjectExpectation{}))
	t.Cleanup(SetTaskUpdaterForTest(func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
		if id == alpha.ID {
			return fmt.Errorf("task with id %q not found", id)
		}
		return nil
	}))

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyCtrlC})

	require.False(t, h.quitting)
	require.False(t, sp.IsEditing(), "the form must close — its row (alpha) is gone")
	require.False(t, sp.IsCreating())
	assert.NotPanics(t, func() { _ = sp.String() })
	assert.Equal(t, []string{"bravo-1"}, taskIDs(sp.GetTasks()), "only alpha is dropped; bravo stays")

	// The cross-task clobber the bug commits writes the discarded task's stale
	// form buffers (Name/Prompt/Cron/etc.) into the neighbour. Assert on every
	// user-editable field the form could write: bravo must still be bravo, not
	// carry alpha's values. (The disk reload renumbers bravo's Ordinal and
	// bumps StoreGeneration when alpha is removed — those are not the bug.)
	bravoAfter := sp.GetTasks()[0]
	assert.Equal(t, bravoBefore.ID, bravoAfter.ID)
	assert.Equal(t, bravoBefore.Name, bravoAfter.Name, "bravo Name unchanged — no stale-buffer write")
	assert.Equal(t, bravoBefore.Prompt, bravoAfter.Prompt, "bravo Prompt unchanged")
	assert.Equal(t, bravoBefore.CronExpr, bravoAfter.CronExpr, "bravo CronExpr unchanged")
	assert.Equal(t, bravoBefore.WatchCmd, bravoAfter.WatchCmd)
	assert.Equal(t, bravoBefore.TargetSession, bravoAfter.TargetSession)
	assert.Equal(t, bravoBefore.OnComplete, bravoAfter.OnComplete)
	assert.Equal(t, bravoBefore.ProjectPath, bravoAfter.ProjectPath)
	assert.Equal(t, bravoBefore.Program, bravoAfter.Program)
	assert.Equal(t, bravoBefore.Enabled, bravoAfter.Enabled)
	assert.Equal(t, "bravo", bravoAfter.Name, "bravo is still bravo — not 'alpha'")
	assert.Equal(t, "bravo prompt", bravoAfter.Prompt)
	assert.Equal(t, "0 10 * * *", bravoAfter.CronExpr)
	// With the form closed, the editor is no longer "editing bravo" with
	// alpha's stale buffers; the render shows the list, not the form.
	assert.NotContains(t, sp.String(), "Edit task bravo-1",
		"the wrongly-re-targeted form is gone — bravo is not 'being edited'")

	text, _ := h.errBox.RetainedNotice()
	assert.Contains(t, text, `Discarded unsaved edits to "alpha"`)
	// No field-level patch is queued for bravo: the next save must not write
	// alpha's stale buffers into the neighbour (the #1700 clobber).
	assert.Empty(t, sp.ConsumeDirty(), "no corruption patch is queued for bravo")
	assert.False(t, sp.IsDirty(), "the pane is clean — alpha was discarded, bravo untouched")
}
