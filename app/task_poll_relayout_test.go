package app

import (
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// pollTasks builds n distinct tasks, the shape a snapshot poll hands
// refreshTasks after an out-of-band `af tasks add|remove`.
func pollTasks(n int) []task.Task {
	tasks := make([]task.Task, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, task.Task{
			ID:       fmt.Sprintf("t%d", i),
			Name:     fmt.Sprintf("task-%d", i),
			CronExpr: "0 0 * * *",
			Prompt:   "echo hi",
			Enabled:  true,
		})
	}
	return tasks
}

// fullAutomationsRows is the automations section's height when the rail has
// room for every row: title, one row per task, the focused row's detail line,
// and the bottom margin (ui/layout/grid.go).
func fullAutomationsRows(n int) int { return 2 + 1 + n }

// TestRefreshTasksReflowsRailOnFirstTask is the #4965 regression guard: a
// CLI-added first task reaches the TUI on the poll, and the rail's automations
// section must appear on that same poll, not on the next unrelated reflow.
func TestRefreshTasksReflowsRailOnFirstTask(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 120, 40)
	require.False(t, h.lastLayout.AutomationsVisible, "precondition: no tasks, no section")

	require.True(t, h.refreshTasks(pollTasks(1), nil))
	require.True(t, h.lastLayout.AutomationsVisible,
		"a poll that adds the first task must reflow the rail so the section appears")
	require.Equal(t, fullAutomationsRows(1), h.lastLayout.Automations.H)
}

// TestRefreshTasksReflowsRailOnAddedTask: with the section already shown, a
// poll that adds a task must grow it on that poll so the new row is not
// clipped.
func TestRefreshTasksReflowsRailOnAddedTask(t *testing.T) {
	h := newTestHome(t)
	h.store.SetTasks(pollTasks(3))
	resizeHome(h, 120, 40)
	require.Equal(t, fullAutomationsRows(3), h.lastLayout.Automations.H,
		"precondition: the rail has room for every row")

	require.True(t, h.refreshTasks(pollTasks(4), nil))
	require.Equal(t, fullAutomationsRows(4), h.lastLayout.Automations.H,
		"a poll that adds a task must grow the section so the new row is not clipped")
}

// TestRefreshTasksReflowsRailOnLastTaskRemoved: a poll that removes the last
// task must drop the section on that poll rather than leave it standing empty.
func TestRefreshTasksReflowsRailOnLastTaskRemoved(t *testing.T) {
	h := newTestHome(t)
	h.store.SetTasks(pollTasks(2))
	resizeHome(h, 120, 40)
	require.True(t, h.lastLayout.AutomationsVisible, "precondition: section shown")

	require.True(t, h.refreshTasks(nil, nil))
	require.False(t, h.lastLayout.AutomationsVisible,
		"a poll that removes the last task must reflow the rail so the section goes")
}

// TestRefreshTasksSameCountDoesNotReflow: the poll runs every 750ms, and a
// change that keeps the task count (an edit, a last-run status bump) leaves
// every grid input alone, so it must not relayout. The terminal size is moved
// behind the grid's back: a relayout would pick it up, so an unchanged
// lastLayout proves none ran.
func TestRefreshTasksSameCountDoesNotReflow(t *testing.T) {
	h := newTestHome(t)
	h.store.SetTasks(pollTasks(2))
	resizeHome(h, 120, 40)
	before := h.lastLayout

	h.termWidth, h.termHeight = 100, 30
	edited := pollTasks(2)
	edited[1].Name = "renamed"
	require.True(t, h.refreshTasks(edited, nil), "an edit is still a visible change")
	require.Equal(t, before, h.lastLayout, "a same-count poll must not relayout")
	require.False(t, h.refreshTasks(edited, nil), "an unchanged poll is not a change")
	require.Equal(t, before, h.lastLayout, "an unchanged poll must not relayout")
}
