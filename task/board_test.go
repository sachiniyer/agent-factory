package task

import (
	"encoding/json"
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

func TestBoardLinkTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test task", "backlog")

	err := b.LinkTask(tk.ID, "my-session")
	assert.NoError(t, err)
	assert.Equal(t, "my-session", b.Tasks[0].InstanceTitle)
}

func TestBoardLinkTaskNotFound(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	assert.Error(t, b.LinkTask("nonexistent", "my-session"))
}

func TestBoardUnlinkTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test task", "backlog")
	b.LinkTask(tk.ID, "my-session")

	err := b.UnlinkTask(tk.ID)
	assert.NoError(t, err)
	assert.Empty(t, b.Tasks[0].InstanceTitle)
}

func TestBoardUnlinkTaskNotFound(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	assert.Error(t, b.UnlinkTask("nonexistent"))
}

func TestBoardFindTaskByInstance(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test task", "backlog")
	b.LinkTask(tk.ID, "my-session")

	found := b.FindTaskByInstance("my-session")
	assert.NotNil(t, found)
	assert.Equal(t, tk.ID, found.ID)

	assert.Nil(t, b.FindTaskByInstance("nonexistent"))
}

func TestBoardLinkSurvivesMove(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Linked task", "backlog")
	b.LinkTask(tk.ID, "my-session")

	// Move the task — link should persist
	err := b.MoveTask(tk.ID, "in_progress")
	assert.NoError(t, err)
	assert.Equal(t, "in_progress", b.Tasks[0].Status)
	assert.Equal(t, "my-session", b.Tasks[0].InstanceTitle)

	// FindTaskByInstance should still work
	found := b.FindTaskByInstance("my-session")
	assert.NotNil(t, found)
	assert.Equal(t, "in_progress", found.Status)
}

func TestBoardLinkJSON(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Linked task", "backlog")
	b.LinkTask(tk.ID, "my-session")

	// Marshal and unmarshal
	data, err := json.MarshalIndent(b, "", "  ")
	assert.NoError(t, err)

	var b2 Board
	err = json.Unmarshal(data, &b2)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(b2.Tasks))
	assert.Equal(t, "my-session", b2.Tasks[0].InstanceTitle)

	// Unlinked task should not have instance_title in JSON
	b.UnlinkTask(tk.ID)
	data, err = json.MarshalIndent(b, "", "  ")
	assert.NoError(t, err)
	assert.NotContains(t, string(data), "instance_title")
}

func TestBoardToggleTask(t *testing.T) {
	b := &Board{Columns: DefaultColumns}
	tk := b.AddTask("Test", "backlog")

	assert.NoError(t, b.ToggleTask(tk.ID))
	assert.Equal(t, "done", b.Tasks[0].Status)

	assert.NoError(t, b.ToggleTask(tk.ID))
	assert.Equal(t, "backlog", b.Tasks[0].Status)
}
