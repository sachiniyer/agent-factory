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
