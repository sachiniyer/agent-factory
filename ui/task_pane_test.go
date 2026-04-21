package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
)

// TestTaskPaneSetTasksEmptyResetsSelectedIdx verifies that calling SetTasks
// with an empty slice leaves selectedIdx at a valid value (0) rather than -1.
// Regression test for #251.
func TestTaskPaneSetTasksEmptyResetsSelectedIdx(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
	})

	// Move selection off index 0 so the clamp logic applies.
	tp.selectedIdx = 1

	// External modification empties the list.
	tp.SetTasks([]task.Task{})
	assert.Equal(t, 0, tp.selectedIdx, "selectedIdx should reset to 0 for an empty list")
}

// TestTaskPaneSetTasksClampsSelectedIdx verifies the existing clamp behavior
// when shrinking a non-empty list.
func TestTaskPaneSetTasksClampsSelectedIdx(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
	})
	tp.selectedIdx = 2

	tp.SetTasks([]task.Task{{ID: "a"}})
	assert.Equal(t, 0, tp.selectedIdx)
}

// TestTaskPaneConsumePendingTriggerEmpty verifies that ConsumePendingTrigger
// returns nil (instead of panicking) when the task list is empty, even if
// selectedIdx is negative. Regression test for #251.
func TestTaskPaneConsumePendingTriggerEmpty(t *testing.T) {
	tp := NewTaskPane()
	// Simulate the legacy broken state where selectedIdx was set to -1.
	tp.selectedIdx = -1
	tp.pendingTrigger = true

	assert.NotPanics(t, func() {
		got := tp.ConsumePendingTrigger()
		assert.Nil(t, got)
	})
	assert.False(t, tp.pendingTrigger, "pendingTrigger should be cleared")
}

// TestTaskPaneConsumePendingTriggerReturnsSelected verifies that
// ConsumePendingTrigger still returns the selected task when valid.
func TestTaskPaneConsumePendingTriggerReturnsSelected(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
	})
	tp.selectedIdx = 1
	tp.pendingTrigger = true

	got := tp.ConsumePendingTrigger()
	if assert.NotNil(t, got) {
		assert.Equal(t, "b", got.ID)
	}
	assert.False(t, tp.pendingTrigger)
}
