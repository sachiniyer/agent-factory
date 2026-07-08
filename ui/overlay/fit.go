package overlay

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

func fitOverlayContent(preferredW, preferredH, maxW, maxH int, style lipgloss.Style) layout.Rect {
	return layout.FitContentRect(
		layout.Rect{W: preferredW, H: preferredH},
		layout.Rect{W: maxW, H: maxH},
		style.GetHorizontalBorderSize(),
		style.GetVerticalBorderSize(),
	)
}

func overlayTextRect(styleRect layout.Rect, style lipgloss.Style) layout.Rect {
	text := layout.Rect{
		W: styleRect.W - style.GetHorizontalPadding(),
		H: styleRect.H - style.GetVerticalPadding(),
	}
	if styleRect.W > 0 && text.W < 1 {
		text.W = 1
	}
	if styleRect.H > 0 && text.H < 1 {
		text.H = 1
	}
	return text
}

func wrapOverlayLines(s string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	wrapped := xansi.Wrap(s, width, " ")
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = truncateOverlayLine(lines[i], width)
	}
	return lines
}

func truncateOverlayLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	out := xansi.Truncate(s, width, "…")
	if strings.Contains(out, "\x1b") {
		out += "\x1b[0m"
	}
	return out
}

func renderedLineCount(s string) int {
	return strings.Count(s, "\n") + 1
}
