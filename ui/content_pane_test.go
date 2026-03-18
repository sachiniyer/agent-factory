package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestContentPaneModeSwitch(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	assert.Equal(t, ContentModeEmpty, cp.GetMode())

	cp.SetMode(ContentModeInstance)
	assert.Equal(t, ContentModeInstance, cp.GetMode())

	cp.SetMode(ContentModeBoard)
	assert.Equal(t, ContentModeBoard, cp.GetMode())

	cp.SetMode(ContentModeTasks)
	assert.Equal(t, ContentModeTasks, cp.GetMode())

}

func TestContentPaneFocus(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	// No focus initially
	assert.False(t, cp.HasFocus())

	// Switch to board mode
	cp.SetMode(ContentModeBoard)
	assert.False(t, cp.HasFocus())

	// Enter focuses the task pane
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	consumed := cp.HandleKeyPress(msg)
	assert.True(t, consumed)
	assert.True(t, cp.HasFocus())

	// Esc releases focus
	escMsg := tea.KeyMsg{Type: tea.KeyEscape}
	consumed = cp.HandleKeyPress(escMsg)
	assert.True(t, consumed)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneTaskFocus(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	cp.SetMode(ContentModeTasks)
	assert.False(t, cp.HasFocus())

	// Enter focuses task pane
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}
	consumed := cp.HandleKeyPress(msg)
	assert.True(t, consumed)
	assert.True(t, cp.HasFocus())
}

func TestContentPaneModeSwitchUnfocuses(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	// Focus task pane
	cp.SetMode(ContentModeBoard)
	cp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, cp.HasFocus())

	// Switch mode should unfocus
	cp.SetMode(ContentModeInstance)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneRender(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	tw.SetSize(80, 30)
	cp := NewContentPane(tw)
	cp.SetSize(80, 30)

	// Empty mode
	cp.SetMode(ContentModeEmpty)
	rendered := cp.String()
	assert.Contains(t, rendered, "Select an item")

	// Instance mode should render the tabbed window
	cp.SetMode(ContentModeInstance)
	rendered = cp.String()
	assert.NotEmpty(t, rendered)

	// Board mode (kanban board)
	cp.SetMode(ContentModeBoard)
	rendered = cp.String()
	assert.Contains(t, rendered, "Board")
}
