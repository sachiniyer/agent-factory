package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestTaskPaneListRecoveryKeepsTitleAndEscHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "empty"},
		{name: "load failure", err: errors.New("task file is unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := NewTaskPane()
			pane.SetSize(60, 12)
			pane.SetFocus(true)
			pane.SetUnavailable(tc.err)

			plain := stripANSI(pane.String())
			require.Contains(t, plain, "Tasks", "the overlay keeps its title")
			lines := strings.Split(plain, "\n")
			require.Equal(t, "n new · esc back", strings.TrimSpace(lines[len(lines)-1]),
				"the recovery footer advertises exactly the live actions")
			require.NotContains(t, plain, "enter", "an empty list must not advertise edit")
			require.NotContains(t, plain, "?", "an empty list must not advertise unavailable actions")

			pane.HandleKeyPress(keyRunes("?"))
			plain = stripANSI(pane.String())
			lines = strings.Split(plain, "\n")
			require.Equal(t, "n new · esc back", strings.TrimSpace(lines[len(lines)-1]),
				"the unavailable action menu must stay hidden in a recovery state")
			require.NotContains(t, plain, "enter")
			require.NotContains(t, plain, "?")
		})
	}
}

func TestTaskPaneUnavailableDisablesSelectionActions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		key              string
		checkUnavailable func(t *testing.T, pane *TaskPane)
		checkAvailable   func(t *testing.T, pane *TaskPane)
	}{
		{
			name: "toggle",
			key:  "x",
			checkUnavailable: func(t *testing.T, pane *TaskPane) {
				require.True(t, pane.GetTasks()[0].Enabled)
			},
			checkAvailable: func(t *testing.T, pane *TaskPane) {
				require.False(t, pane.GetTasks()[0].Enabled)
			},
		},
		{
			name: "run",
			key:  "r",
			checkUnavailable: func(t *testing.T, pane *TaskPane) {
				require.False(t, pane.HasPendingTrigger())
			},
			checkAvailable: func(t *testing.T, pane *TaskPane) {
				require.True(t, pane.HasPendingTrigger())
			},
		},
		{
			name: "delete",
			key:  "D",
			checkUnavailable: func(t *testing.T, pane *TaskPane) {
				require.Len(t, pane.GetTasks(), 2)
			},
			checkAvailable: func(t *testing.T, pane *TaskPane) {
				require.Len(t, pane.GetTasks(), 1)
			},
		},
		{
			name: "edit",
			key:  "enter",
			checkUnavailable: func(t *testing.T, pane *TaskPane) {
				require.False(t, pane.IsEditing())
			},
			checkAvailable: func(t *testing.T, pane *TaskPane) {
				require.True(t, pane.IsEditing())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := NewTaskPane()
			pane.SetTasks([]task.Task{
				{ID: "first", Name: "first", Enabled: true},
				{ID: "second", Name: "second", Enabled: true},
			})
			pane.SetFocus(true)
			pane.SetUnavailable(errors.New("task file is unreadable"))

			pane.HandleKeyPress(keyRunes(tc.key))
			tc.checkUnavailable(t, pane)

			pane.SetUnavailable(nil)
			pane.HandleKeyPress(keyRunes(tc.key))
			tc.checkAvailable(t, pane)
		})
	}
}

func TestTaskPaneSelectedTaskUnavailable(t *testing.T) {
	pane := NewTaskPane()
	pane.SetTasks([]task.Task{
		{ID: "first", Name: "first"},
		{ID: "second", Name: "second"},
	})
	pane.SetUnavailable(errors.New("task file is unreadable"))

	_, ok := pane.SelectedTask()
	require.False(t, ok, "recovery mode must not expose a retained selection")

	pane.SetUnavailable(nil)
	selected, ok := pane.SelectedTask()
	require.True(t, ok)
	require.Equal(t, "first", selected.ID)
}

func TestTaskPaneUnavailableOpenEditorPreservesEditsAndBlocksActions(t *testing.T) {
	for _, key := range []string{"r", "x", "D"} {
		t.Run(key, func(t *testing.T) {
			pane := NewTaskPane()
			pane.SetTasks([]task.Task{{ID: "first", Name: "first", Enabled: true}, {ID: "second", Name: "second"}})
			pane.SetFocus(true)
			pane.EnterEditSelected()
			pane.editName.SetValue("unsaved")
			pane.SetUnavailable(errors.New("task file is unreadable"))
			pane.HandleKeyPress(keyRunes(key))
			require.False(t, pane.HasPendingTrigger())
			require.Len(t, pane.GetTasks(), 2)
			require.True(t, pane.GetTasks()[0].Enabled)
			require.Empty(t, pane.ConsumeDeleted())
			require.False(t, pane.IsDirty())
			require.True(t, pane.IsEditing())
			require.Equal(t, "unsaved", pane.editName.Value())
			pane.HandleKeyPress(keyRunes("z"))
			require.Contains(t, pane.editName.Value(), "z", "text editing continues during failed refresh")
			pane.SetUnavailable(nil)
			pane.HandleKeyPress(keyRunes(key))
			switch key {
			case "r":
				require.True(t, pane.HasPendingTrigger())
			case "x":
				require.False(t, pane.GetTasks()[0].Enabled)
			case "D":
				require.Len(t, pane.GetTasks(), 1)
			}
		})
	}
}

func TestTaskPaneUnavailableBlocksSubmission(t *testing.T) {
	pane := NewTaskPane()
	original := task.Task{ID: "first", Name: "first", Prompt: "do it", CronExpr: "* * * * *", ProjectPath: newGitRepo(t), Program: "claude"}
	pane.SetTasks([]task.Task{original})
	pane.SetFocus(true)
	pane.EnterEditSelected()
	pane.editName.SetValue("unsaved")
	pane.SetUnavailable(errors.New("task file is unreadable"))
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, original, pane.GetTasks()[0])
	require.False(t, pane.IsDirty())
	require.True(t, pane.IsEditing())
	pane.focusIndex = taskFocusPrompt
	pane.updateEditFocus()
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Contains(t, pane.editPrompt.Value(), "\n")
	pane.focusIndex = taskFocusName
	pane.updateEditFocus()
	pane.SetUnavailable(nil)
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, "unsaved", pane.GetTasks()[0].Name)
	require.True(t, pane.IsDirty())
	require.False(t, pane.IsEditing())
}

func TestTaskPaneUnavailableMutators(t *testing.T) {
	for _, action := range []string{"delete ID", "delete selected", "toggle", "run"} {
		t.Run(action, func(t *testing.T) {
			pane := NewTaskPane()
			original := []task.Task{{ID: "first", Name: "first", Enabled: true}, {ID: "second", Name: "second"}}
			pane.SetTasks(append([]task.Task(nil), original...))
			mutate := func() {
				switch action {
				case "delete ID":
					pane.DeleteTask("second")
				case "delete selected":
					pane.deleteSelectedTask()
				case "toggle":
					pane.toggleSelectedTask()
				case "run":
					pane.runSelectedTask()
				}
			}
			pane.SetUnavailable(errors.New("task file is unreadable"))
			mutate()
			require.Equal(t, original, pane.GetTasks())
			require.Equal(t, 0, pane.selectedIdx)
			require.False(t, pane.IsDirty())
			require.False(t, pane.HasPendingTrigger())
			require.Empty(t, pane.ConsumeDeleted())
			pane.SetUnavailable(nil)
			mutate()
			if action == "run" {
				require.True(t, pane.HasPendingTrigger())
			} else {
				require.True(t, pane.IsDirty())
			}
		})
	}
}
