package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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

func TestTaskPaneUnavailableAllowsCreation(t *testing.T) {
	pane := NewTaskPane()
	pane.createPath = newGitRepo(t)
	pane.SetFocus(true)
	pane.SetUnavailable(errors.New("task file is unreadable"))
	pane.HandleKeyPress(keyRunes("n"))
	require.True(t, pane.IsCreating())
	pane.editName.SetValue("new task")
	pane.editPrompt.SetValue("do it")
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, pane.HasPendingCreate(), "recovery must allow creating a new task")
	require.Empty(t, pane.GetTasks(), "creation must not mutate retained task data")
	require.False(t, pane.IsDirty())
}

func TestTaskPaneUnavailableEditorExplainsLiveActions(t *testing.T) {
	for _, height := range []int{10, 40} {
		pane := NewTaskPane()
		pane.SetSize(100, height)
		pane.SetTasks([]task.Task{{ID: "first", Name: "first", Prompt: "original prompt"}})
		pane.SetFocus(true)
		pane.EnterEditSelected()
		pane.editName.SetValue("unsaved name")
		pane.editPrompt.SetValue("unsaved prompt")
		pane.SetUnavailable(errors.New("task file is unreadable"))
		plain := stripANSI(pane.String())
		require.Contains(t, plain, "Cannot load tasks")
		require.Contains(t, plain, "task file is unreadable")
		require.Contains(t, strings.ReplaceAll(plain, "\n", " "), "changes cannot be saved until the next successful refresh")
		for _, dead := range []string{"r run now ·", "r run ·", "D delete", "x toggle", "enter save"} {
			require.NotContains(t, plain, dead)
		}
		require.Contains(t, plain, "tab fields · typing · esc back")
		require.LessOrEqual(t, len(strings.Split(plain, "\n")), height)
		pane.SetUnavailable(nil)
		pane.SetSize(100, 40)
		plain = stripANSI(pane.String())
		require.Contains(t, plain, "enter save")
		require.Contains(t, plain, "x toggle")
		require.NotContains(t, plain, "Cannot load tasks")
		require.Equal(t, "unsaved name", pane.editName.Value())
		require.Equal(t, "unsaved prompt", pane.editPrompt.Value())
	}
}

func TestTaskPaneUnavailableSanitizesControlBytes(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	for _, editing := range []bool{false, true} {
		pane := NewTaskPane()
		pane.SetSize(100, 40)
		pane.SetTasks([]task.Task{{ID: "first", Name: "first"}})
		pane.SetFocus(true)
		if editing {
			pane.EnterEditSelected()
		}
		pane.SetUnavailable(errors.New("bad\x1b[31mpath\r\b\a\f\v\x7f\u0085"))
		rendered := pane.String()
		require.Contains(t, rendered, "badpath")
		require.NotContains(t, rendered, "\x1b", "error controls must not reach either renderer")
		require.NotContains(t, rendered, "\r")
		for _, b := range []byte(rendered) {
			require.False(t, b < 0x20 && b != '\n' && b != '\t', "control byte %#x survived", b)
		}
		for _, r := range rendered {
			require.False(t, r >= 0x7f && r <= 0x9f, "control rune U+%04X survived", r)
		}
	}
}
