package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4487: a reload (SetTasks) reconciles the pane
// with disk instead of discarding what the user is holding, so the app can run
// it unconditionally.

func reloadTask(id, prompt string) task.Task {
	return task.Task{ID: id, Name: id, Prompt: prompt, CronExpr: "* * * * *", Enabled: true}
}

// load returns a fresh slice per call, as every read of tasks.json does, so a
// test cannot pass by a pane edit writing through into the "disk" it reloads.
func load(tasks ...task.Task) func() []task.Task {
	return func() []task.Task { return append([]task.Task(nil), tasks...) }
}

func paneIDs(s *TaskPane) []string {
	ids := make([]string, 0, len(s.tasks))
	for _, t := range s.tasks {
		ids = append(ids, t.ID)
	}
	return ids
}

// failSave drives the pane through saveContentPaneState's sequence with every
// daemon write refused before it commits: the edits are restored for retry and
// the drained deletions are simply gone from the queue.
func failSave(s *TaskPane) {
	for _, edit := range s.ConsumeDirty() {
		s.RestoreFailedEdit(edit.ID)
	}
	s.ConsumeDeleted()
}

// A reload keeps a retained edit's value and baseline while every row the user
// did not touch takes the loaded record, so the next save still sends only the
// field the user changed — never the other writer's prompt.
func TestTaskPaneReloadKeepsEditAndRefreshesUntouchedRows(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	disk := []task.Task{reloadTask("a", "cli"), reloadTask("b", "cli"), reloadTask("c", "p")}
	s.SetTasks(disk)

	assert.Equal(t, []string{"a", "b", "c"}, paneIDs(s), "a task added on disk must appear")
	assert.False(t, s.tasks[0].Enabled, "the retained edit must survive the reload")
	assert.Equal(t, "p", s.tasks[0].Prompt, "an edited row keeps the values its edit was made against")
	assert.Equal(t, "cli", s.tasks[1].Prompt, "an untouched row must take the loaded record")
	assert.True(t, s.IsDirty(), "the retained edit still needs saving")

	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	assert.Equal(t, "a", edits[0].ID)
	require.NotNil(t, edits[0].Update.Enabled)
	assert.False(t, *edits[0].Update.Enabled)
	assert.Nil(t, edits[0].Update.Prompt, "the patch must not carry a field the user never edited (#1700)")
}

// #4257 and #4488 at pane level: a delete that fails while an edit is retained
// must leave the task visible exactly once, however many times in a row it
// fails, with the edit still retained.
func TestTaskPaneReloadAfterRepeatedFailedDeletesShowsEachTaskOnce(t *testing.T) {
	disk := load(reloadTask("keep", "p"), reloadTask("gone", "p"))
	s := NewTaskPane()
	s.SetTasks(disk())
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	for attempt := 1; attempt <= 3; attempt++ {
		require.True(t, s.DeleteTask("gone"), "attempt %d: the task must be there to delete", attempt)
		require.Equal(t, []string{"keep"}, paneIDs(s), "attempt %d: a queued deletion is hidden", attempt)
		failSave(s)
		s.SetTasks(disk())

		require.Equal(t, []string{"keep", "gone"}, paneIDs(s),
			"attempt %d: a task whose deletion failed is still on disk and must show exactly once", attempt)
		assert.False(t, s.tasks[0].Enabled, "attempt %d: the retained edit must survive", attempt)
		assert.True(t, s.IsDirty(), "attempt %d: the retained edit still needs saving", attempt)
		assert.Empty(t, s.deleted, "attempt %d: a failed deletion is not retried behind the user's back", attempt)
	}
}

// A background reload must not cancel a deletion the user queued but has not
// saved yet, nor bring the row back.
func TestTaskPaneReloadKeepsQueuedDeletion(t *testing.T) {
	disk := load(reloadTask("a", "p"), reloadTask("b", "p"))
	s := NewTaskPane()
	s.SetTasks(disk())
	s.SetFocus(true)
	require.True(t, s.DeleteTask("b"))

	s.SetTasks(disk())

	assert.Equal(t, []string{"a"}, paneIDs(s))
	assert.True(t, s.IsDirty())
	deleted := s.ConsumeDeleted()
	require.Len(t, deleted, 1)
	assert.Equal(t, "b", deleted[0].ID)
}

// An open edit form survives a reload that moves its task, stays bound to that
// task by ID, and saves a patch diffed against the record the form opened on.
func TestTaskPaneReloadKeepsOpenEditFormBoundToItsTask(t *testing.T) {
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
	s.SelectTask(1)
	s.EnterEditSelected()
	require.True(t, s.IsEditing())
	s.editName.SetValue("renamed")

	// Another writer adds a task ahead of b and changes b's prompt.
	s.SetTasks([]task.Task{
		withRepo(reloadTask("new", "p")), withRepo(reloadTask("a", "p")), withRepo(reloadTask("b", "cli")),
	})

	require.True(t, s.IsEditing(), "a reload must not close the form")
	assert.Equal(t, "renamed", s.editName.Value(), "a reload must not touch the form's text")
	selected, ok := s.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "b", selected.ID, "the form must stay bound to the task it opened on")

	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.False(t, s.IsEditing(), "Enter saves the form")
	assert.Equal(t, []string{"new", "a", "b"}, paneIDs(s))
	assert.Equal(t, "renamed", s.tasks[2].Name, "the form must write into its own task")
	assert.Equal(t, "a", s.tasks[1].Name, "no other row may receive the form")

	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	assert.Equal(t, "b", edits[0].ID)
	require.NotNil(t, edits[0].Update.Name)
	assert.Equal(t, "renamed", *edits[0].Update.Name)
	assert.Nil(t, edits[0].Update.Prompt,
		"the other writer's prompt must not be overwritten by the form's stale copy (#1700)")
}

// A task deleted on disk while the user holds an edit to it stays visible after
// the loaded rows, so the draft is not dropped silently; deleting it discards
// the draft and the next reload lets it go.
func TestTaskPaneReloadKeepsDraftOfTaskGoneFromDisk(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	disk := load(reloadTask("b", "p"), reloadTask("c", "p"))
	s.SetTasks(disk())

	require.Equal(t, []string{"b", "c", "a"}, paneIDs(s),
		"loaded rows keep disk order, so a rail index still names the same task")
	assert.False(t, s.tasks[2].Enabled, "the draft must survive")
	selected, ok := s.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "a", selected.ID, "the cursor must follow the task it was on")

	require.True(t, s.HandleKeyPress(keyRunes("D")))
	assert.Equal(t, []string{"b", "c"}, paneIDs(s))
	assert.Empty(t, s.ConsumeDirty(), "a deleted draft is not also saved")
	deleted := s.ConsumeDeleted()
	require.Len(t, deleted, 1, "discarding the draft is an ordinary deletion")
	assert.Equal(t, "a", deleted[0].ID)
	s.SetTasks(disk())
	assert.Equal(t, []string{"b", "c"}, paneIDs(s))
	assert.False(t, s.IsDirty())
}

// The cursor follows its task when a reload inserts a row above it, so the
// next key acts on the task the user is looking at.
func TestTaskPaneReloadCursorFollowsTask(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SelectTask(1)

	s.SetTasks([]task.Task{reloadTask("new", "p"), reloadTask("a", "p"), reloadTask("b", "p")})

	selected, ok := s.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "b", selected.ID)
}

// The pane must own its rows. The app hands the same loaded slice to the rail,
// and an unsaved toggle must not show up there.
func TestTaskPaneReloadDoesNotAliasLoadedSlice(t *testing.T) {
	loaded := []task.Task{reloadTask("a", "p")}
	s := NewTaskPane()
	s.SetTasks(loaded)
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	assert.True(t, loaded[0].Enabled, "a pane edit must not write through to the caller's slice")
}

// A damaged tasks.json that lists an ID twice must not copy a held draft into
// two rows the user could edit or delete separately.
func TestTaskPaneReloadPlacesHeldDraftOnce(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("a", "p")})

	assert.Equal(t, []string{"a", "b"}, paneIDs(s))
	assert.False(t, s.tasks[0].Enabled)
}

// When a damaged file lists an ID twice, the copy the user edited is the draft
// the reload keeps, whichever copy that is; keeping an untouched copy would
// drop the edit while the pane still claimed to be dirty.
func TestTaskPaneReloadKeepsTheEditedCopyOfADuplicatedID(t *testing.T) {
	for _, edited := range []int{0, 1} {
		s := NewTaskPane()
		s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("a", "p")})
		s.SetFocus(true)
		s.SelectTask(edited)
		require.True(t, s.HandleKeyPress(keyRunes("x")))

		s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("a", "p")})

		require.Equal(t, []string{"a", "b"}, paneIDs(s), "copy %d", edited)
		assert.False(t, s.tasks[0].Enabled, "copy %d: the copy the user toggled must be the one kept", edited)
		edits := s.ConsumeDirty()
		require.Len(t, edits, 1, "copy %d: the toggle must still be saved", edited)
		require.NotNil(t, edits[0].Update.Enabled)
		assert.False(t, *edits[0].Update.Enabled)
	}
}

// The gone-from-load half of TestTaskPaneReloadKeepsTheEditedCopyOfADuplicatedID:
// when a damaged file lists an ID twice and the user's edit is on a copy that is
// not the first-iterated one, a reload that omits that ID must keep the copy
// heldRows selected — not the iterated s.tasks copy. A genuine deletion on disk,
// a scoped/partial reload, or a transient read failure all hit this path. Keeping
// the unedited copy would make ConsumeDirty emit nothing (the kept copy matches
// its baseline), so the next reload drops the orphaned row and the edit vanishes
// with no signal — the silent drop SetTasks' docstring and the #4798 notice
// machinery exist to prevent.
func TestTaskPaneReloadKeepsEditedCopyOfDuplicatedIDAbsentFromLoad(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("a", "p")})
	require.Equal(t, []string{"a", "b", "a"}, paneIDs(s))
	s.SetFocus(true)
	s.SelectTask(2) // the user edits the second copy of "a", not the first-iterated one
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	// The reload omits "a" entirely: deleted on disk, a scoped reload, or a
	// transient read failure. Only the second loop of SetTasks runs for it.
	s.SetTasks([]task.Task{reloadTask("b", "p")})

	require.Equal(t, []string{"b", "a"}, paneIDs(s),
		"the draft a reload cannot see stays after the loaded rows")
	require.False(t, s.tasks[1].Enabled,
		"the edited copy must be kept; the unedited first copy would silently lose the draft")
	assert.True(t, s.IsDirty(), "the retained edit still needs saving")
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1, "the toggle must still be saved (otherwise silent draft loss)")
	assert.Equal(t, "a", edits[0].ID)
	require.NotNil(t, edits[0].Update.Enabled)
	assert.False(t, *edits[0].Update.Enabled)
}
