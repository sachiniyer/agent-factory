package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4798: once a save proves a draft's task deleted,
// DiscardDeletedDraft drops the draft with one notice naming it, instead of
// keeping a row whose save fails with "not found" on every close.

// discardSave drives saveContentPaneState's sequence for a task another writer
// deleted: the update answers "not found", so the edit is discarded rather than
// restored, and the save reloads a list without the task.
func discardSave(s *TaskPane, deletedID string, disk []task.Task) {
	for _, edit := range s.ConsumeDirty() {
		if edit.ID == deletedID {
			s.DiscardDeletedDraft(edit.ID)
		} else {
			s.RestoreFailedEdit(edit.ID)
		}
	}
	s.ConsumeDeleted()
	s.SetTasks(disk)
}

func TestTaskPaneDiscardDeletedDraftDropsItWithOneNotice(t *testing.T) {
	demo := reloadTask("demo-id", "p")
	demo.Name = "demo"
	s := NewTaskPane()
	s.SetTasks([]task.Task{demo, reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.IsDirty())

	disk := load(reloadTask("b", "p"))
	discardSave(s, "demo-id", disk())

	assert.Equal(t, []string{"b"}, paneIDs(s), "the draft's row must go")
	assert.False(t, s.IsDirty(), "nothing is left to save")
	assert.Equal(t, `Discarded unsaved edits to "demo" — the task was deleted`, s.TakeDiscardedDraftNotice(),
		"the draft is never dropped silently")
	assert.Empty(t, s.TakeDiscardedDraftNotice(), "the notice is raised once")
	for attempt := 1; attempt <= 3; attempt++ {
		assert.Empty(t, s.ConsumeDirty(), "close %d must not retry the save", attempt)
		s.SetTasks(disk())
		assert.Equal(t, []string{"b"}, paneIDs(s))
	}
	assert.Empty(t, s.TakeDiscardedDraftNotice())
}

// Only the proven-deleted draft goes. A draft whose save failed for any other
// reason is retained through the same save, with its values.
func TestTaskPaneDiscardDeletedDraftKeepsOtherDrafts(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}))
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	discardSave(s, "a", []task.Task{reloadTask("b", "cli")})

	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.True(t, s.IsDirty(), "b's failed edit is still waiting to save")
	assert.False(t, s.tasks[0].Enabled, "b keeps its edit")
	assert.Equal(t, "p", s.tasks[0].Prompt, "b keeps the values its edit was made against")
	selected, ok := s.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "b", selected.ID, "the cursor stays on its task")
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	assert.Equal(t, "b", edits[0].ID)
	assert.Equal(t, `Discarded unsaved edits to "a" — the task was deleted`, s.TakeDiscardedDraftNotice())
}

// Absence from a reload is not proof the task was deleted — the reload can be
// scoped to another project or simply be wrong — so a reload alone never drops
// a draft (#4487's contract, which #4798 keeps).
func TestTaskPaneReloadAloneNeverDiscardsADraft(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.SetTasks([]task.Task{reloadTask("b", "p")})

	assert.Equal(t, []string{"b", "a"}, paneIDs(s))
	assert.True(t, s.IsDirty())
	assert.Empty(t, s.TakeDiscardedDraftNotice())
}

// Several drafts dropped before the app takes the notice are named in one
// notice, each by the name it was loaded with rather than a rename it carried.
func TestTaskPaneDiscardedDraftNoticeNamesEveryDraft(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("c", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}))
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	s.tasks[1].Name = "renamed"

	s.DiscardDeletedDraft("a")
	s.DiscardDeletedDraft("b")

	assert.Equal(t, []string{"c"}, paneIDs(s))
	assert.Equal(t, `Discarded unsaved edits to "a", "b" — the tasks were deleted`, s.TakeDiscardedDraftNotice())
}

// A draft whose edit was reverted carries nothing a save would send, so
// dropping it loses no work and raises no notice; an unknown ID is a no-op.
func TestTaskPaneDiscardDeletedDraftWithoutWorkRaisesNoNotice(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.HandleKeyPress(keyRunes("x")))

	s.DiscardDeletedDraft("a")
	s.DiscardDeletedDraft("never-loaded")

	assert.Equal(t, []string{"b"}, paneIDs(s))
	assert.False(t, s.IsDirty())
	assert.Empty(t, s.TakeDiscardedDraftNotice())
}
