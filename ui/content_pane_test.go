package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestContentPaneModeSwitch(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewDiffPane(), NewTerminalPane(), nil)
	cp := NewContentPane(tw, nil)

	assert.Equal(t, ContentModeEmpty, cp.GetMode())

	cp.SetMode(ContentModeInstance)
	assert.Equal(t, ContentModeInstance, cp.GetMode())

	cp.SetMode(ContentModeTodos)
	assert.Equal(t, ContentModeTodos, cp.GetMode())

	cp.SetMode(ContentModeSchedules)
	assert.Equal(t, ContentModeSchedules, cp.GetMode())

	cp.SetMode(ContentModeMicroClaw)
	assert.Equal(t, ContentModeMicroClaw, cp.GetMode())
}

func TestContentPaneFocus(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewDiffPane(), NewTerminalPane(), nil)
	cp := NewContentPane(tw, nil)

	// No focus initially
	assert.False(t, cp.HasFocus())

	// Switch to todos mode
	cp.SetMode(ContentModeTodos)
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

func TestContentPaneScheduleFocus(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewDiffPane(), NewTerminalPane(), nil)
	cp := NewContentPane(tw, nil)

	cp.SetMode(ContentModeSchedules)
	assert.False(t, cp.HasFocus())

	// Enter focuses schedule pane
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}
	consumed := cp.HandleKeyPress(msg)
	assert.True(t, consumed)
	assert.True(t, cp.HasFocus())
}

func TestContentPaneModeSwitchUnfocuses(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewDiffPane(), NewTerminalPane(), nil)
	cp := NewContentPane(tw, nil)

	// Focus task pane
	cp.SetMode(ContentModeTodos)
	cp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, cp.HasFocus())

	// Switch mode should unfocus
	cp.SetMode(ContentModeInstance)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneRender(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewDiffPane(), NewTerminalPane(), nil)
	tw.SetSize(80, 30)
	cp := NewContentPane(tw, nil)
	cp.SetSize(80, 30)

	// Empty mode
	cp.SetMode(ContentModeEmpty)
	rendered := cp.String()
	assert.Contains(t, rendered, "Select an item")

	// Instance mode should render the tabbed window
	cp.SetMode(ContentModeInstance)
	rendered = cp.String()
	assert.NotEmpty(t, rendered)

	// Todos mode
	cp.SetMode(ContentModeTodos)
	rendered = cp.String()
	assert.Contains(t, rendered, "Tasks")
}
