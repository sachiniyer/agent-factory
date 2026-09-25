package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4824 against #4798: an edit whose save could not
// be confirmed is kept, but no automatic save re-sends it.

// heldUnconfirmed returns a pane whose edit to "demo" (Enabled true → false)
// went through one save that could not be confirmed.
func heldUnconfirmed(t *testing.T) *TaskPane {
	t.Helper()
	demo := reloadTask("demo", "p")
	s := NewTaskPane()
	s.SetTasks([]task.Task{demo})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	s.HoldUnconfirmedEdit(edits[0].ID)
	return s
}

func TestTaskPaneUnconfirmedEditIsKeptButNotResent(t *testing.T) {
	s := heldUnconfirmed(t)
	s.SetTasks(load(reloadTask("demo", "p"))()) // the re-read: the edit did not land

	assert.True(t, s.IsDirty(), "the draft is kept (#4798)")
	require.Len(t, s.GetTasks(), 1)
	assert.False(t, s.GetTasks()[0].Enabled, "the edited value survives the reload")
	assert.Empty(t, s.ConsumeDirty(), "an automatic save does not re-send it")
	assert.True(t, s.IsDirty(), "and it is still held after that save")
	assert.Empty(t, s.TakeSettledDraftNotice())
}

// A new edit is the explicit re-save: the edit form's Enter marks the task
// dirty the same way, and the whole patch goes out again.
func TestTaskPaneUnconfirmedEditIsSentAfterANewEdit(t *testing.T) {
	s := heldUnconfirmed(t)
	s.markTaskDirty("demo")

	edits := s.ConsumeDirty()
	require.Len(t, edits, 1)
	require.NotNil(t, edits[0].Update.Enabled)
	assert.False(t, *edits[0].Update.Enabled, "the kept edit is part of the re-sent patch")
}

// The re-read shows the edit landed: settle it as clean, and say so.
func TestTaskPaneUnconfirmedEditSettlesWhenTheReloadCarriesIt(t *testing.T) {
	s := heldUnconfirmed(t)
	landed := reloadTask("demo", "p")
	landed.Enabled = false
	s.SetTasks(load(landed)())

	assert.False(t, s.IsDirty())
	assert.Equal(t, `Saved edits to "demo" — the task list now shows them`, s.TakeSettledDraftNotice())
	assert.Empty(t, s.TakeSettledDraftNotice(), "the notice is raised once")
	assert.Empty(t, s.TakeUnconfirmedQuitNotice(), "nothing is left to warn about")
}

func TestTaskPaneUnconfirmedQuitNoticeIsRaisedOnce(t *testing.T) {
	s := heldUnconfirmed(t)
	assert.Contains(t, s.TakeUnconfirmedQuitNotice(), `Edits to "demo" could not be confirmed`)
	assert.Empty(t, s.TakeUnconfirmedQuitNotice(), "the next quit goes through")
}
