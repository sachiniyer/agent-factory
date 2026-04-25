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

	cp.SetMode(ContentModeTasks)
	assert.Equal(t, ContentModeTasks, cp.GetMode())

}

func TestContentPaneFocus(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	// No focus initially
	assert.False(t, cp.HasFocus())

	// Switch to tasks mode
	cp.SetMode(ContentModeTasks)
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
	cp.SetMode(ContentModeTasks)
	cp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, cp.HasFocus())

	// Switch mode should unfocus
	cp.SetMode(ContentModeInstance)
	assert.False(t, cp.HasFocus())
}

func TestContentPaneSetSizeMatchesRenderHeight(t *testing.T) {
	tw := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	cp := NewContentPane(tw)

	const w, h = 80, 30
	cp.SetSize(w, h)

	// renderInlinePane uses Place height = c.height - windowStyle.GetVerticalFrameSize() - 2.
	// The child panes must be sized to match, otherwise content is cut off
	// or padded incorrectly (issue #336).
	expected := h - windowStyle.GetVerticalFrameSize() - 2
	assert.Equal(t, expected, cp.taskPane.height,
		"taskPane height must match renderInlinePane Place height")
	assert.Equal(t, expected, cp.hooksPane.height,
		"hooksPane height must match renderInlinePane Place height")
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
}
