package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderCenteredFallback renders fallback text horizontally and vertically
// centered within a width×height content box, returning exactly height lines.
//
// The text is rendered (and therefore wrapped) at the target width before the
// vertical padding is computed, so centering uses the wrapped line count
// rather than the pre-wrap count — on narrow panes the fallback ASCII art
// wraps and padding computed from the raw line count miscenters it (#699).
//
// When the wrapped text is taller than the box it is clamped to the bottom
// height lines — keeping the trailing message visible, matching the
// keep-newest truncation both panes use in normal mode — so a fallback can
// never overflow its allocation and push the menu/error box off-screen.
//
// Callers pass their pane's full content dimensions: TabbedWindow.SetSize has
// already stripped tab-bar and window-frame chrome, so subtracting chrome
// here would double-count it (#703, #616).
func renderCenteredFallback(style lipgloss.Style, text string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}

	rendered := style.Width(width).Align(lipgloss.Center).Render(text)
	lines := strings.Split(rendered, "\n")
	if len(lines) >= height {
		return strings.Join(lines[len(lines)-height:], "\n")
	}

	topPadding := (height - len(lines)) / 2
	out := make([]string, 0, height)
	out = append(out, make([]string, topPadding)...)
	out = append(out, lines...)
	out = append(out, make([]string, height-len(lines)-topPadding)...)
	return strings.Join(out, "\n")
}
