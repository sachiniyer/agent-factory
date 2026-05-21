package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestHooksPaneEditModeCtrlCCancels(t *testing.T) {
	h := NewHooksPane()
	h.SetCommands([]string{"make test"})
	h.SetFocus(true)

	assert.True(t, h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	assert.True(t, h.editing)

	assert.True(t, h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC}))
	assert.False(t, h.editing)
	assert.False(t, h.adding)
	assert.Empty(t, h.editBuffer)
}

func TestHooksPaneSetCommandsEmptySliceBug(t *testing.T) {
	h := NewHooksPane()
	h.SetCommands([]string{"cmd1", "cmd2", "cmd3"})
	h.SetFocus(true)
	h.ScrollDown() // selectedIdx: 0 -> 1
	h.ScrollDown() // selectedIdx: 1 -> 2

	h.SetCommands([]string{})         // selectedIdx = -1
	h.SetCommands([]string{"newcmd"}) // selectedIdx stays -1

	defer func() {
		if r := recover(); r != nil {
			t.Logf("Bug confirmed: panic occurred: %v", r)
		}
	}()

	h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // PANIC
}

// TestHooksPaneSetCommandsRoundtripKeepsSelectedIdxValid verifies that going
// non-empty -> empty -> non-empty leaves selectedIdx in range so subsequent
// indexing is safe. Regression test for #615.
func TestHooksPaneSetCommandsRoundtripKeepsSelectedIdxValid(t *testing.T) {
	h := NewHooksPane()
	h.SetCommands([]string{"cmd1", "cmd2", "cmd3"})
	h.SetFocus(true)
	h.ScrollDown()
	h.ScrollDown()
	assert.Equal(t, 2, h.selectedIdx)

	h.SetCommands([]string{})
	assert.Equal(t, 0, h.selectedIdx, "selectedIdx should reset to 0 for an empty list")

	h.SetCommands([]string{"newcmd"})
	assert.GreaterOrEqual(t, h.selectedIdx, 0, "selectedIdx must not be negative")
	assert.Less(t, h.selectedIdx, len(h.GetCommands()), "selectedIdx must be in range")

	assert.NotPanics(t, func() {
		h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	})
	assert.True(t, h.editing)
	assert.Equal(t, "newcmd", h.editBuffer)
}
