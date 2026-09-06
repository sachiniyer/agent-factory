package ui

import "github.com/charmbracelet/lipgloss"

// SelectionMarker keeps the active cursor distinct from the selected row's ink.
// Accent on SurfaceRaised is contrast-checked by the generated token contract.
func SelectionMarker(marker string) string {
	t := CurrentTheme()
	return lipgloss.NewStyle().Bold(true).Foreground(t.Accent).Background(t.SurfaceRaised).Render(marker)
}
