package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4798: a draft whose task another writer deleted
// (`af tasks remove`) can never be saved, so a reload drops it with one notice
// naming it instead of keeping a row that retries the save forever.

// A reload that no longer contains the draft's task drops the draft, leaves
// nothing to save, and queues exactly one notice naming the task.
func TestTaskPaneReloadDropsDraftOfDeletedTask(t *testing.T) {
	demo := reloadTask("demo-id", "p")
	demo.Name = "demo"
	s := NewTaskPane()
	s.SetTasks([]task.Task{demo, reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.IsDirty())

	disk := load(reloadTask("b", "p"), reloadTask("c", "p"))
	s.SetTasks(disk())

	assert.Equal(t, []string{"b", "c"}, paneIDs(s), "the orphaned draft must not stay in the list")
	assert.False(t, s.IsDirty(), "nothing is left to save")
	assert.Empty(t, s.ConsumeDirty(), "a save must not retry an update of a deleted task")
	assert.False(t, s.HasUnsavedEdit("demo-id"))
	assert.Equal(t, `Discarded unsaved edits to "demo" — the task was deleted`, s.TakeDiscardedDraftNotice(),
		"the draft is never dropped silently")
	assert.Empty(t, s.TakeDiscardedDraftNotice(), "the notice is raised once")

	s.SetTasks(disk())
	assert.Equal(t, []string{"b", "c"}, paneIDs(s))
	assert.Empty(t, s.TakeDiscardedDraftNotice(), "a later reload has nothing more to report")
}

// A save that fails because the task is gone reconciles on the reload that
// follows it: the draft is dropped, the next save sends nothing, and the pane
// is not dirty — so closing the overlay stops reporting "not found".
func TestTaskPaneFailedSaveOfDeletedTaskIsNotRetried(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	disk := load(reloadTask("b", "p"))
	failSave(s) // the update is refused: task with id "a" not found
	require.True(t, s.HasUnsavedEdit("a"), "a failed save retains the edit until the reload")
	s.SetTasks(disk())

	assert.False(t, s.HasUnsavedEdit("a"), "the reload shows the task is gone, so the edit is moot")
	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.False(t, s.IsDirty())
	assert.Equal(t, `Discarded unsaved edits to "a" — the task was deleted`, s.TakeDiscardedDraftNotice())
	for attempt := 1; attempt <= 3; attempt++ {
		assert.Empty(t, s.ConsumeDirty(), "close %d must not retry the save", attempt)
		s.SetTasks(disk())
	}
}

// A draft of a task that still exists is kept exactly as before (#4487): only
// the draft whose task is gone is dropped.
func TestTaskPaneReloadKeepsDraftWhoseTaskStillExists(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}))
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.SetTasks([]task.Task{reloadTask("b", "cli")})

	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.True(t, s.HasUnsavedEdit("b"), "the surviving task's draft must be kept")
	assert.Equal(t, "p", s.tasks[0].Prompt, "the kept draft keeps its own values")
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	assert.Equal(t, "b", edits[0].ID)
	assert.Equal(t, `Discarded unsaved edits to "a" — the task was deleted`, s.TakeDiscardedDraftNotice())
}

// Several drafts dropped before the app takes the notice are named in one
// notice rather than the first one being overwritten.
func TestTaskPaneDiscardedDraftNoticeNamesEveryDraft(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("c", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}))
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.SetTasks([]task.Task{reloadTask("b", "p"), reloadTask("c", "p")})
	s.SetTasks([]task.Task{reloadTask("c", "p")})

	assert.Equal(t, []string{"c"}, paneIDs(s))
	assert.Equal(t, `Discarded unsaved edits to "a", "b" — the tasks were deleted`, s.TakeDiscardedDraftNotice())
}

// A draft whose edit was reverted carries nothing a save would send, so
// dropping it loses no work and raises no notice.
func TestTaskPaneReloadDropsRevertedDraftWithoutNotice(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.SetTasks([]task.Task{reloadTask("b", "p")})

	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.False(t, s.IsDirty())
	assert.Empty(t, s.TakeDiscardedDraftNotice())
}

// An open edit form bound to a task deleted on disk is not pulled out from
// under the user: its row stays until the form closes. Once the form writes
// into it, the next reload drops the draft with the notice.
func TestTaskPaneReloadKeepsOpenFormOfDeletedTaskUntilItCloses(t *testing.T) {
	repo := newGitRepo(t)
	withRepo := func(tk task.Task) task.Task {
		tk.ProjectPath = repo
		tk.Program = "claude"
		return tk
	}
	s := NewTaskPane()
	s.SetSize(100, 40)
	s.SetTasks([]task.Task{withRepo(reloadTask("a", "p")), withRepo(reloadTask("b", "p"))})
	s.SetFocus(true)
	s.EnterEditSelected()
	require.True(t, s.IsEditing())
	s.editName.SetValue("renamed")

	disk := load(withRepo(reloadTask("b", "p")))
	s.SetTasks(disk())

	require.True(t, s.IsEditing(), "a reload must not close the form")
	assert.Equal(t, []string{"b", "a"}, paneIDs(s), "the form's row stays after the loaded rows")
	assert.Empty(t, s.TakeDiscardedDraftNotice(), "nothing was dropped yet")

	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.False(t, s.IsEditing())
	s.SetTasks(disk())

	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.False(t, s.IsDirty())
	assert.Equal(t, `Discarded unsaved edits to "a" — the task was deleted`, s.TakeDiscardedDraftNotice(),
		"the notice names the task as it was loaded, not the draft's rename")
}
