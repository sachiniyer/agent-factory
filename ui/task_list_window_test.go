package ui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestTaskDetailsKeepLastSelectedTaskVisible(t *testing.T) {
	pane := NewTaskPane()
	pane.SetSize(70, 12)
	pane.SetFocus(true)
	var tasks []task.Task
	for i := 0; i < 30; i++ {
		tasks = append(tasks, task.Task{ID: fmt.Sprint(i), Name: fmt.Sprintf("task-%02d", i), Prompt: "Selected task detail", CronExpr: "0 9 * * *"})
	}
	pane.SetTasks(tasks)
	for i := 1; i < len(tasks); i++ {
		pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	}
	out := pane.String()
	require.Contains(t, out, "task-29")
	require.Contains(t, out, "Selected task detail")
	require.Contains(t, out, "enter edit")
	require.NotContains(t, out, "task-00")
	require.Equal(t, 12, renderedLineCount(out))
}
