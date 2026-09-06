package ui

import (
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

// renderConfigInput assigns every input span explicitly: an outer background
// cannot restore the selected surface after a nested foreground style resets it.
func renderConfigInput(input *textinput.Model) string {
	t := CurrentTheme()
	input.TextStyle = configSelectedStyle
	input.PromptStyle = configSelectedStyle
	input.PlaceholderStyle = configSelectedStyle.Foreground(t.InkMuted)
	input.CompletionStyle = input.PlaceholderStyle
	input.Cursor.TextStyle = configSelectedStyle
	// Bubbles reverses its cursor style internally; this emits Surface on Accent.
	input.Cursor.Style = lipgloss.NewStyle().Foreground(t.Accent).Background(t.Surface)
	return input.View()
}
