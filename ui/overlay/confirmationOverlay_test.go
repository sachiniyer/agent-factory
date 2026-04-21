package overlay

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestConfirmationOverlay_HandleKeyPress_CtrlC(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	ctrlCMsg := tea.KeyMsg{Type: tea.KeyCtrlC}
	shouldClose := overlay.HandleKeyPress(ctrlCMsg)

	assert.True(t, shouldClose, "ctrl+c should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called")
	assert.False(t, confirmCalled, "OnConfirm should not be called")
}

func TestConfirmationOverlay_HandleKeyPress_Esc(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	escMsg := tea.KeyMsg{Type: tea.KeyEsc}
	shouldClose := overlay.HandleKeyPress(escMsg)

	assert.True(t, shouldClose, "esc should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, cancelCalled, "OnCancel should be called")
}

func TestConfirmationOverlay_HandleKeyPress_ConfirmKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	yMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}
	shouldClose := overlay.HandleKeyPress(yMsg)

	assert.True(t, shouldClose, "confirm key should close the overlay")
	assert.True(t, overlay.Dismissed, "overlay should be dismissed")
	assert.True(t, confirmCalled, "OnConfirm should be called")
}

func TestConfirmationOverlay_HandleKeyPress_OtherKey(t *testing.T) {
	overlay := NewConfirmationOverlay("Test confirmation")

	cancelCalled := false
	overlay.OnCancel = func() {
		cancelCalled = true
	}

	confirmCalled := false
	overlay.OnConfirm = func() {
		confirmCalled = true
	}

	otherMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}
	shouldClose := overlay.HandleKeyPress(otherMsg)

	assert.False(t, shouldClose, "other keys should not close the overlay")
	assert.False(t, overlay.Dismissed, "overlay should not be dismissed")
	assert.False(t, cancelCalled, "OnCancel should not be called")
	assert.False(t, confirmCalled, "OnConfirm should not be called")
}
