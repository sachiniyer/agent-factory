package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

func TestTaskPaneNormalModeAllowsQuitKeysToPropagate(t *testing.T) {
	tp := NewTaskPane()
	tp.SetFocus(true)

	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}))
	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC}))
}

// TestTaskPaneCreateModeCapturesProgram drives the create form with the same
// key events the bubbletea runtime would deliver, confirming that the Program
// field is captured into the pending-create payload alongside the existing
// fields. Regression test for #453.
func TestTaskPaneCreateModeCapturesProgram(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")

	typeRunes := func(runes string) {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(runes)})
	}

	// Name
	typeRunes("daily")
	// Move to prompt, cron, path, program (program is focus index 4).
	for i := 0; i < 4; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	// Replace the default path with a valid value isn't necessary — path
	// already carries the default. Type into the program field.
	typeRunes("aider")
	// Set a valid cron by jumping back to the cron field.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	// We're on cron now (index 2); type a valid expression.
	typeRunes("* * * * *")
	// Walk forward past path and program to the submit button (index 5).
	for i := 0; i < 3; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	name, _, cron, path, program := tp.ConsumePendingCreate()
	assert.Equal(t, "daily", name)
	assert.Equal(t, "* * * * *", cron)
	assert.Equal(t, "/tmp/repo", path)
	assert.Equal(t, "aider", program, "Program field must be carried through to the pending-create payload")
}

// TestTaskPaneEditModePersistsProgramChange confirms that editing an existing
// task's Program field via the form writes the change back to the task slice
// so the caller persists it on save. Regression test for #453.
func TestTaskPaneEditModePersistsProgramChange(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "old-name",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: "/tmp/repo",
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Walk to the Program field (index 4).
	for i := 0; i < 4; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	// Clear by overwriting: textinput.Update treats backspace as delete.
	for i := 0; i < len("claude"); i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("aider")})
	// Tab to the Save button (index 5) and press enter.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	assert.True(t, tp.IsDirty(), "edit should mark the pane dirty")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "aider", tasks[0].Program, "Program field must reflect the edited value")
	}
}
