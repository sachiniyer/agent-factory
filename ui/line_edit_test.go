package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/schedule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctrlU = tea.KeyMsg{Type: tea.KeyCtrlU}

func typeRunes(send func(tea.KeyMsg), s string) {
	for _, r := range s {
		send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestEditLine(t *testing.T) {
	cases := []struct {
		name, in string
		msg      tea.KeyMsg
		want     string
		ok       bool
	}{
		{"ctrl+u clears", "gam", ctrlU, "", true},
		{"ctrl+u on empty", "", ctrlU, "", true},
		{"backspace drops a rune", "gé", tea.KeyMsg{Type: tea.KeyBackspace}, "g", true},
		{"backspace on empty", "", tea.KeyMsg{Type: tea.KeyBackspace}, "", true},
		{"space appends", "a", tea.KeyMsg{Type: tea.KeySpace}, "a ", true},
		{"runes append", "a", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("bc")}, "abc", true},
		{"other keys pass through", "a", tea.KeyMsg{Type: tea.KeyLeft}, "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := EditLine(tc.in, tc.msg)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

// TestHooksPaneEditCtrlUClears pins #4846 for the post-worktree hook editor, a
// hand-rolled field that dropped ctrl+u on the floor.
func TestHooksPaneEditCtrlUClears(t *testing.T) {
	h := NewHooksPane()
	h.SetCommands([]string{"make test"})
	h.SetFocus(true)
	require.True(t, h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.True(t, h.editing)
	require.Equal(t, "make test", h.editBuffer)

	h.HandleKeyPress(ctrlU)
	typeRunes(func(m tea.KeyMsg) { h.HandleKeyPress(m) }, "zzz")
	assert.Equal(t, "zzz", h.editBuffer, "ctrl+u clears the command before new typing")
}

// The bubbles-backed fields already honour ctrl+u through their default
// keymaps; these guard that the forms keep forwarding it (#4846), so the key
// means the same thing in every TUI text field.

func TestTaskFormFieldsCtrlUClears(t *testing.T) {
	tp := NewTaskPane()
	tp.initForm(nil, "")
	send := func(m tea.KeyMsg) { tp.HandleKeyPress(m) }

	tp.focusIndex = taskFocusName
	tp.updateEditFocus()
	typeRunes(send, "nightly")
	send(ctrlU)
	assert.Empty(t, tp.editName.Value(), "task name")

	tp.focusIndex = taskFocusPrompt
	tp.updateEditFocus()
	typeRunes(send, "run it")
	send(ctrlU)
	assert.Empty(t, tp.editPrompt.Value(), "task prompt")

	tp.focusIndex = taskFocusPath
	tp.updateEditFocus()
	typeRunes(send, "/repo")
	send(ctrlU)
	assert.Empty(t, tp.editPath.Value(), "task path")
}

func TestSchedulePickerCustomCronCtrlUClears(t *testing.T) {
	p := newSchedulePicker()
	p.setType(schedule.Custom)
	p.setFocused(true)
	for p.activeCell() != cellRaw {
		p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	p.raw.SetValue("0 7 21 9 *")
	p.raw.CursorEnd()
	p.handleKey(ctrlU)
	assert.Empty(t, p.raw.Value())
}

func TestConfigEditorCtrlUClears(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "network.listen_addr")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, c.IsEditing())
	c.input.SetValue("")
	typeInto(c, "127.0.0.1:8080")

	c.HandleKeyPress(ctrlU)
	assert.Empty(t, c.input.Value())
}
