package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ResetTasks is the reload for a different scope: nothing the user held
// against the old list may follow it into the new one.
func TestTaskPaneResetTasksDiscardsHeldState(t *testing.T) {
	s := NewTaskPane()
	s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "p"), reloadTask("c", "p")})
	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	require.True(t, s.DeleteTask("b"))
	s.SelectTask(1)
	s.EnterEditSelected()
	require.True(t, s.IsEditing())
	s.pendingCreate = true

	s.ResetTasks([]task.Task{reloadTask("other", "p")})

	assert.Equal(t, []string{"other"}, paneIDs(s), "no row from the old scope may survive")
	assert.False(t, s.IsDirty())
	assert.False(t, s.IsEditing())
	assert.False(t, s.HasPendingCreate())
	assert.Empty(t, s.ConsumeDirty())
	assert.Empty(t, s.ConsumeDeleted())
	assert.Equal(t, 0, s.selectedIdx)
}

// SetTasks reports a change only when the rows or the cursor moved, so the
// poll does not repaint a pane whose retained draft keeps it different from
// disk.
func TestTaskPaneSetTasksReportsChange(t *testing.T) {
	disk := load(reloadTask("a", "p"), reloadTask("b", "p"))
	s := NewTaskPane()
	assert.True(t, s.SetTasks(disk()), "a first load is a change")
	assert.False(t, s.SetTasks(disk()), "the same list is not a change")

	s.SetFocus(true)
	require.True(t, s.HandleKeyPress(keyRunes("x")))
	assert.False(t, s.SetTasks(disk()), "a held draft that differs from disk is not a new change")

	assert.True(t, s.SetTasks([]task.Task{reloadTask("a", "p"), reloadTask("b", "cli")}),
		"a changed untouched row is a change")
	assert.True(t, s.SetTasks([]task.Task{reloadTask("new", "p"), reloadTask("a", "p"), reloadTask("b", "cli")}),
		"a row inserted above the cursor is a change")
	s.SetTasks(nil)
	assert.False(t, s.SetTasks(nil), "an empty reload repeated is not a change")
}
