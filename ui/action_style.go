package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ActionStyle paints the complete label, so a control reads as one affordance.
// No added cells: compact overlays and their mouse zones retain their budgets.
func ActionStyle(primary bool) lipgloss.Style {
	t := CurrentTheme()
	style := lipgloss.NewStyle().Foreground(t.Ink).Background(t.Surface)
	if primary {
		return style.Bold(true).Foreground(t.Surface).Background(t.Accent)
	}
	return style
}

// ActionHint uses the shared secondary treatment for each footer verb.
func ActionHint(hint string) string {
	parts := strings.Split(hint, " · ")
	for i, part := range parts {
		parts[i] = ActionStyle(false).Render(part)
	}
	return strings.Join(parts, " · ")
}
