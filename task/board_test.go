package task

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBoardAddTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test task", "backlog")

	assert.Equal(t, "Test task", tk.Title)
	assert.Equal(t, "backlog", tk.Status)
	assert.Equal(t, 1, b.TaskCount())
}

func TestBoardMoveTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test", "backlog")

	err := b.MoveTask(tk.ID, "in_progress")
	assert.NoError(t, err)
	assert.Equal(t, "in_progress", b.Tasks[0].Status)

	err = b.MoveTask(tk.ID, "done")
	assert.NoError(t, err)
	assert.Equal(t, "done", b.Tasks[0].Status)
}

func TestBoardMoveTaskNotFound(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	assert.Error(t, b.MoveTask("nonexistent", "done"))
}

func TestBoardDeleteTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	t1 := b.AddTask("Task 1", "backlog")
	b.AddTask("Task 2", "backlog")

	assert.NoError(t, b.DeleteTask(t1.ID))
	assert.Equal(t, 1, b.TaskCount())
	assert.Equal(t, "Task 2", b.Tasks[0].Title)
}

func TestBoardDeleteTaskNotFound(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	assert.Error(t, b.DeleteTask("nonexistent"))
}

func TestBoardGetTasksByStatus(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	b.AddTask("Task 1", "backlog")
	b.AddTask("Task 2", "in_progress")
	b.AddTask("Task 3", "backlog")

	assert.Len(t, b.GetTasksByStatus("backlog"), 2)
	assert.Len(t, b.GetTasksByStatus("in_progress"), 1)
	assert.Nil(t, b.GetTasksByStatus("review"))
}

func TestBoardCountByStatus(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	b.AddTask("Task 1", "backlog")
	b.AddTask("Task 2", "in_progress")
	b.AddTask("Task 3", "backlog")
	b.AddTask("Task 4", "done")

	counts := b.CountByStatus()
	assert.Equal(t, 2, counts["backlog"])
	assert.Equal(t, 1, counts["in_progress"])
	assert.Equal(t, 1, counts["done"])
}

func TestBoardToggleTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test", "backlog")

	assert.NoError(t, b.ToggleTask(tk.ID))
	assert.Equal(t, "done", b.Tasks[0].Status)

	assert.NoError(t, b.ToggleTask(tk.ID))
	assert.Equal(t, "backlog", b.Tasks[0].Status)
}
