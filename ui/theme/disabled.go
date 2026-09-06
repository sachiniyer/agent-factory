package theme

import "github.com/charmbracelet/lipgloss"

// Disabled keeps readable body ink and distinguishes an unavailable control by
// its dashed outline, following the buttons/fields recipe in the design guide.
func Disabled() lipgloss.Style {
	return Styles().Body.Border(lipgloss.Border{
		Top: "┄", Bottom: "┄", Left: "┆", Right: "┆",
		TopLeft: "┌", TopRight: "┐", BottomLeft: "└", BottomRight: "┘",
	}).BorderForeground(Roles().Border).Padding(0, Metrics()["space-2"])
}
