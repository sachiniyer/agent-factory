package ui

import (
	"strings"
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

// fillCreateForm types a name, prompt, and cron into the create form so
// submitting via the Save button doesn't trip validation. Leaves focus on
// index 0 (Name) so callers can walk to whichever field they want to drive
// next.
func fillCreateForm(t *testing.T, tp *TaskPane, name string) {
	t.Helper()
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do something")})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> cron
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	// Walk back to name (index 0) so callers can navigate forward consistently.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
}

// TestTaskPaneCreateModeRejectsEmptyPrompt is the regression guard for #517:
// submitting the create form with no prompt (or whitespace-only) must surface
// an inline validation error instead of marking a pending create with a blank
// prompt that no-ops when the scheduler fires.
func TestTaskPaneCreateModeRejectsEmptyPrompt(t *testing.T) {
	for _, prompt := range []string{"", "   "} {
		t.Run("prompt="+prompt, func(t *testing.T) {
			tp := NewTaskPane()
			tp.EnterCreateMode("/tmp/repo")

			// Fill name, leave prompt empty/whitespace, fill cron.
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("daily")})
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
			if prompt != "" {
				tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(prompt)})
			}
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> cron
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})

			// Walk to Save and submit.
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> path
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> program
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> save
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

			assert.False(t, tp.HasPendingCreate(), "empty prompt must not produce a pending create")
			assert.True(t, tp.IsCreating(), "form must stay open so user can fix the error")
			assert.Equal(t, "prompt must be non-empty", tp.editError,
				"inline validation error must surface to the user")
		})
	}
}

// TestTaskPaneCreateModeSelectorDefaultsToConfigDefault verifies that creating
// a new task without touching the Program selector persists "" so the daemon
// uses the configured default_program. Regression test for #492.
func TestTaskPaneCreateModeSelectorDefaultsToConfigDefault(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")
	fillCreateForm(t, tp, "daily")

	// Walk to the Save button (index 5) without touching the Program selector.
	for i := 0; i < 5; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	_, _, _, _, program := tp.ConsumePendingCreate()
	assert.Equal(t, "", program, "default selector option must persist an empty Program")
}

// TestTaskPaneCreateModeSelectorPicksCanonicalAgent verifies that advancing
// the Program selector to a SupportedPrograms entry persists the canonical
// bare name (no path, no flags). Regression test for #492.
func TestTaskPaneCreateModeSelectorPicksCanonicalAgent(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")
	fillCreateForm(t, tp, "daily")

	// Walk to the Program field (index 4) and step the selector to "claude"
	// (option index 1: 0 is the default sentinel, 1 is the first supported).
	for i := 0; i < 4; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	// Advance to the Save button and submit.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	_, _, _, _, program := tp.ConsumePendingCreate()
	assert.Equal(t, "claude", program, "selector must persist the canonical agent name")
}

// TestTaskPaneEditModePresetFromExistingProgram verifies that opening edit
// mode on a task whose Program already matches a SupportedPrograms entry
// pre-selects that option so saving without changes is a no-op. Regression
// test for #492.
func TestTaskPaneEditModePresetFromExistingProgram(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: "/tmp/repo",
		Program:     "aider",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Tab to Save and submit without touching the selector.
	for i := 0; i < 5; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "aider", tasks[0].Program,
			"pre-selected canonical option must round-trip the original value")
	}
}

// TestTaskPaneEditModePreservesCustomProgram verifies that opening edit mode
// on a task whose Program is a legacy free-text command preserves the raw
// value verbatim — saving without explicit change must not collapse it to
// a canonical agent name (#492).
func TestTaskPaneEditModePreservesCustomProgram(t *testing.T) {
	tp := NewTaskPane()
	const raw = "/usr/local/bin/aider --model gpt-4"
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: "/tmp/repo",
		Program:     raw,
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Tab to Save and submit without touching the selector.
	for i := 0; i < 5; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, raw, tasks[0].Program,
			"legacy free-text program must be preserved verbatim across edit/save")
	}
}

// TestTaskPaneEditModeCtrlCCancels is the regression guard for #526: Ctrl+C
// inside the edit form must cancel the edit (matching Esc) instead of being
// silently swallowed. Dirty buffer changes must not be written back.
func TestTaskPaneEditModeCtrlCCancels(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Dirty the Name buffer so we can prove the cancel doesn't persist edits.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("XXX")})

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.False(t, tp.IsEditing(), "Ctrl+C should exit edit mode")
	assert.Equal(t, "", tp.editError, "Ctrl+C should clear any inline error")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "nightly", tasks[0].Name,
			"Ctrl+C must not write the dirty Name buffer back to the task")
	}
}

// TestTaskPaneCreateModeCtrlCCancels mirrors the edit-mode regression guard
// for the create form: Ctrl+C must exit create mode without producing a
// pending create. Regression test for #526.
func TestTaskPaneCreateModeCtrlCCancels(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.False(t, tp.IsCreating(), "Ctrl+C should exit create mode")
	assert.False(t, tp.HasPendingCreate(), "Ctrl+C must not produce a pending create")
}

// TestTaskPaneListShowsAgentNameNotFullProgram confirms the list view collapses
// a noisy program string (path + flags) down to the agent name for #455.
func TestTaskPaneListShowsAgentNameNotFullProgram(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "/usr/local/bin/aider --model gpt-4",
		Enabled:     true,
	}})

	out := tp.String()
	assert.Contains(t, out, "aider", "list view should render the agent name")
	assert.False(t, strings.Contains(out, "/usr/local/bin/aider"),
		"list view must not include the full program path")
	assert.False(t, strings.Contains(out, "--model gpt-4"),
		"list view must not include program flags")
}
