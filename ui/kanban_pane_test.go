package ui

import (
	"claude-squad/task"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func makeTestBoard() *task.Board {
	b := &task.Board{Columns: task.DefaultColumns}
	b.AddTask("Backlog task 1", "backlog")
	b.AddTask("Backlog task 2", "backlog")
	b.AddTask("In progress task", "in_progress")
	b.AddTask("Done task", "done")
	return b
}

func TestKanbanPaneBasic(t *testing.T) {
	kp := NewKanbanPane()
	assert.False(t, kp.HasFocus())
	assert.False(t, kp.IsDirty())
	assert.Nil(t, kp.GetBoard())
}

func TestKanbanPaneSetBoard(t *testing.T) {
	kp := NewKanbanPane()
	board := makeTestBoard()
	kp.SetBoard(board)
	assert.Equal(t, 4, kp.GetBoard().TaskCount())
	assert.Len(t, kp.flat, 8) // 4 headers + 4 tasks
}

func TestKanbanPaneNavigation(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetBoard(makeTestBoard())
	kp.SetFocus(true)

	// Start at first item (backlog header)
	assert.Equal(t, 0, kp.selectedIdx)
	assert.True(t, kp.flat[0].isHeader)

	// Move down
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.Equal(t, 1, kp.selectedIdx)
	assert.False(t, kp.flat[1].isHeader)

	// Jump to next column
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	assert.True(t, kp.flat[kp.selectedIdx].isHeader)
	assert.Equal(t, "in_progress", kp.flat[kp.selectedIdx].column)
}

func TestKanbanPaneAddTask(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetBoard(&task.Board{Columns: task.DefaultColumns})
	kp.SetFocus(true)

	// Press 'n' to add
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	assert.True(t, kp.adding)

	// Type title
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new task")})
	assert.Equal(t, "new task", kp.editBuffer)

	// Press Enter to save
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, kp.IsDirty())
	assert.Equal(t, 1, kp.GetBoard().TaskCount())
	assert.Equal(t, "new task", kp.GetBoard().Tasks[0].Title)
	assert.Equal(t, "backlog", kp.GetBoard().Tasks[0].Status)
}

func TestKanbanPaneDelete(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetBoard(makeTestBoard())
	kp.SetFocus(true)

	// Move to first task (index 1, after backlog header)
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.Equal(t, 1, kp.selectedIdx)

	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	assert.True(t, kp.IsDirty())
	assert.Equal(t, 3, kp.GetBoard().TaskCount())
}

func TestKanbanPaneGrabDrop(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetBoard(makeTestBoard())
	kp.SetFocus(true)

	// Move to first task
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	// Grab
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	assert.True(t, kp.carrying)
	assert.NotNil(t, kp.carriedTask)
	assert.Equal(t, 3, kp.GetBoard().TaskCount()) // removed from board

	// Jump to review column
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})

	// Drop
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	assert.False(t, kp.carrying)
	assert.Equal(t, 4, kp.GetBoard().TaskCount())

	// Verify the task moved to review
	reviewTasks := kp.GetBoard().GetTasksByStatus("review")
	assert.Len(t, reviewTasks, 1)
}

func TestKanbanPaneGrabCancel(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetBoard(makeTestBoard())
	kp.SetFocus(true)

	// Move to first task and grab
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	assert.True(t, kp.carrying)

	// Cancel
	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, kp.carrying)
	assert.Equal(t, 4, kp.GetBoard().TaskCount()) // task restored
}

func TestKanbanPaneEscUnfocuses(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetFocus(true)
	assert.True(t, kp.HasFocus())

	kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, kp.HasFocus())
}

func TestKanbanPaneNoConsumeWithoutFocus(t *testing.T) {
	kp := NewKanbanPane()
	assert.False(t, kp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}))
}

func TestKanbanPaneRender(t *testing.T) {
	kp := NewKanbanPane()
	kp.SetSize(80, 30)
	kp.SetBoard(makeTestBoard())

	rendered := kp.String()
	assert.Contains(t, rendered, "Board")
	assert.Contains(t, rendered, "Backlog")
	assert.Contains(t, rendered, "In Progress")
	assert.Contains(t, rendered, "Done")
	assert.Contains(t, rendered, "Backlog task 1")
}
