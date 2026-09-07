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

	h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})

	require.Equal(t, stateTasks, h.state)
	require.Nil(t, h.confirmationOverlay, "unavailable recovery must not confirm a retained task")
}
