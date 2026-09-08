package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestTaskDeleteConfirmationRetainsTargetAndFocus(t *testing.T) {
	h := newTestHome(t)
	h.termWidth, h.termHeight = 120, 36
	h.relayout()
	h.state = stateTasks
	pane := h.automations.TaskPane()
	a := task.Task{ID: "a", Name: "Daily review"}
	b := task.Task{ID: "b", Name: "Issue intake"}
	pane.SetTasks([]task.Task{a, b})
	pane.SetFocus(true)
	deleteKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}
	h.handleStateTasks(deleteKey)
	require.Equal(t, stateConfirm, h.state)
	require.Len(t, pane.GetTasks(), 2, "opening a confirmation must not delete")
	h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateTasks, h.state)
	require.True(t, pane.HasFocus())
	require.Len(t, pane.GetTasks(), 2)
	h.handleStateTasks(deleteKey)
	// A refresh reorders the list while the confirmation is open.
	pane.SetTasks([]task.Task{b, a})
	h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.Equal(t, stateTasks, h.state)
	require.True(t, pane.HasFocus())
	require.Equal(t, []task.Task{b}, pane.GetTasks())
}

func TestUnavailableTasksDoNotOpenDeleteConfirmation(t *testing.T) {
	h := newTestHome(t)
	h.state = stateTasks
	pane := h.automations.TaskPane()
	pane.SetTasks([]task.Task{{ID: "a", Name: "retained"}, {ID: "b", Name: "other"}})
	pane.SetFocus(true)
	pane.SetUnavailable(errors.New("task file is unreadable"))

	h.showTasksOverlay()
	require.False(t, pane.IsEditing(), "unavailable retained tasks must open recovery, not the editor")
	h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})

	require.Equal(t, stateTasks, h.state)
	require.Nil(t, h.confirmationOverlay, "unavailable recovery must not confirm a retained task")
}

func TestUnavailableTasksRefusePendingDeleteConfirmation(t *testing.T) {
	h := newTestHome(t)
	h.termWidth, h.termHeight = 120, 36
	h.relayout()
	h.state = stateTasks
	pane := h.automations.TaskPane()
	pane.SetTasks([]task.Task{{ID: "a", Name: "retained"}, {ID: "b", Name: "other"}})
	pane.SetFocus(true)
	deleteKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}
	confirmKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	h.handleStateTasks(deleteKey)
	require.Equal(t, stateConfirm, h.state)
	pane.SetUnavailable(errors.New("task file is unreadable"))
	h.handleStateConfirm(confirmKey)
	require.Equal(t, stateTasks, h.state)
	require.Len(t, pane.GetTasks(), 2)
	require.Empty(t, pane.ConsumeDeleted())
	require.False(t, pane.IsDirty())
	require.Contains(t, pane.String(), "Cannot load tasks")
	require.Contains(t, pane.String(), "task file is unreadable")
	pane.SetUnavailable(nil)
	h.handleStateTasks(deleteKey)
	require.Equal(t, stateConfirm, h.state)
	h.handleStateConfirm(confirmKey)
	require.Equal(t, stateTasks, h.state)
	require.Len(t, pane.GetTasks(), 1)
	require.Len(t, pane.ConsumeDeleted(), 1)
}

func TestUnavailableTasksEditorCapture(t *testing.T) {
	h := newTestHome(t)
	h.termWidth, h.termHeight = 80, 24
	h.relayout()
	pane := h.automations.TaskPane()
	pane.SetTasks([]task.Task{{ID: "review", Name: "Review changes", Prompt: "Keep my unsaved prompt"}})
	h.showTasksOverlay()
	pane.SetUnavailable(errors.New("task file is unreadable"))
	frame := h.View()
	t.Logf("80x24 unavailable editor:\n%s", frame)
	require.Contains(t, pane.String(), "Cannot load tasks")
	require.NotContains(t, pane.String(), "enter save")
}

func TestUnavailableTasksCreateClearsLoadFailure(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = setupRealRepo(t)
	t.Chdir(h.repoRoot)
	h.termWidth, h.termHeight = 100, 30
	h.relayout()
	pane := h.automations.TaskPane()
	h.showTasksOverlay()
	pane.SetUnavailable(errors.New("task file was unreadable"))
	key := func(msg tea.KeyMsg) { h.handleStateTasks(msg) }
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, pane.IsCreating())
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Recovery task")})
	for i := 0; i < 3; i++ {
		key(tea.KeyMsg{Type: tea.KeyTab})
	}
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Review changes")})
	for i := 0; i < 3; i++ {
		key(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
	key(tea.KeyMsg{Type: tea.KeyEnter})
	require.False(t, pane.IsCreating())
	require.Len(t, pane.GetTasks(), 1, "successful creation reloads the new task")
	require.Equal(t, "Recovery task", pane.GetTasks()[0].Name)
	_, available := pane.SelectedTask()
	require.True(t, available, "a successful reload clears the unavailable state")
	require.NotContains(t, pane.String(), "Cannot load tasks")
	require.Contains(t, pane.String(), "Recovery task")
}
