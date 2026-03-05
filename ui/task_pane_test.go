package ui

import (
	"claude-squad/task"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestTaskPaneBasic(t *testing.T) {
	tp := NewTaskPane()
	assert.False(t, tp.HasFocus())
	assert.False(t, tp.IsDirty())
	assert.Nil(t, tp.GetTasks())
}

func TestTaskPaneSetTasks(t *testing.T) {
	tp := NewTaskPane()
	tasks := []task.Task{
		{ID: "1", Title: "Task 1"},
		{ID: "2", Title: "Task 2", Done: true},
	}
	tp.SetTasks(tasks)
	assert.Len(t, tp.GetTasks(), 2)
}

func TestTaskPaneAddTask(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{})
	tp.SetFocus(true)

	// Press 'n' to add
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	assert.True(t, tp.adding)

	// Type title
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new task")})
	assert.Equal(t, "new task", tp.editBuffer)

	// Press Enter to save
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.IsDirty())
	assert.Len(t, tp.GetTasks(), 1)
	assert.Equal(t, "new task", tp.GetTasks()[0].Title)
}

func TestTaskPaneToggle(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "1", Title: "Task 1", Done: false},
	})
	tp.SetFocus(true)

	// Toggle
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	assert.True(t, tp.GetTasks()[0].Done)
	assert.True(t, tp.IsDirty())
}

func TestTaskPaneDelete(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "1", Title: "Task 1"},
		{ID: "2", Title: "Task 2"},
	})
	tp.SetFocus(true)

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	assert.Len(t, tp.GetTasks(), 1)
	assert.Equal(t, "Task 2", tp.GetTasks()[0].Title)
	assert.True(t, tp.IsDirty())
}

func TestTaskPaneEscUnfocuses(t *testing.T) {
	tp := NewTaskPane()
	tp.SetFocus(true)
	assert.True(t, tp.HasFocus())

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, tp.HasFocus())
}

func TestTaskPaneEditMode(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "1", Title: "Original"},
	})
	tp.SetFocus(true)

	// Enter edit mode
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.editing)
	assert.Equal(t, "Original", tp.editBuffer)

	// Clear and type new title
	tp.editBuffer = ""
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Updated")})

	// Save
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "Updated", tp.GetTasks()[0].Title)
	assert.True(t, tp.IsDirty())
}

func TestTaskPaneNoConsumeWithoutFocus(t *testing.T) {
	tp := NewTaskPane()
	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}))
}

func TestTaskPaneRender(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "1", Title: "My Task", Done: false},
	})

	rendered := tp.String()
	assert.Contains(t, rendered, "Tasks")
	assert.Contains(t, rendered, "My Task")
}
