package ui

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/charmbracelet/lipgloss"
)

// TerminalTooSmall is the banner shown when the terminal is below the layout
// engine's hard minimum (RFC §2.6): no regions are laid out, so the whole
// window renders this centered notice instead. Exactly width×height cells.
func TerminalTooSmall(width, height int) string {
	msg := fmt.Sprintf("Terminal too small\nneed at least %d×%d",
		layout.HardMinWidth, layout.HardMinHeight)
	return layout.ClampToRect(
		renderCenteredFallback(lipgloss.NewStyle(), msg, width, height),
		layout.Rect{W: width, H: height})
}

// renderCenteredFallback renders fallback text horizontally and vertically
// centered within a width×height content box, returning exactly height lines.
//
// The text is rendered (and therefore wrapped) at the target width before the
// vertical padding is computed, so centering uses the wrapped line count
// rather than the pre-wrap count (#699).
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

// paneFallbackContent adds the full Agent Factory logo only when it fits
// without wrapping. At ordinary 80-column terminals the workspace pane is
// narrower than the logo; omitting it keeps transient setup/name fallbacks calm
// and leaves their actionable message intact instead of rendering art fragments
// (#2146).
func paneFallbackContent(message string, width int) string {
	if width < lipgloss.Width(FallBackText) {
		return message
	}
	return lipgloss.JoinVertical(lipgloss.Center, FallBackText, "", message)
}
