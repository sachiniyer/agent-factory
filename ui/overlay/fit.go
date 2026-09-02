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
	if layout.Cells(s) <= width {
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

// selectionWindow returns a window containing selected with at most maxVisible
// rows from a list of total rows.
func selectionWindow(selected, total, maxVisible int) (start, end int) {
	if maxVisible < 1 {
		maxVisible = 1
	}
	if total <= maxVisible {
		return 0, total
	}
	if selected >= maxVisible {
		start = selected - maxVisible + 1
	}
	end = start + maxVisible
	if end > total {
		end = total
		start = end - maxVisible
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

// budgetedSelectionWindow shrinks the data window until its rows and scroll
// indicators fit within available. maxVisible limits the data rows when
// positive; zero leaves them limited only by available. If even one data row
// plus its indicators cannot fit, the selected row wins and indicators drop.
func budgetedSelectionWindow(selected, total, available, maxVisible int) (start, end int, showAbove, showBelow bool) {
	if total == 0 {
		return 0, 0, false, false
	}
	if available < 1 {
		start, end = selectionWindow(selected, total, 1)
		return start, end, false, false
	}

	rows := available
	if maxVisible > 0 && rows > maxVisible {
		rows = maxVisible
	}
	for rows > 0 {
		start, end = selectionWindow(selected, total, rows)
		showAbove = start > 0
		showBelow = end < total
		need := end - start
		if showAbove {
			need++
		}
		if showBelow {
			need++
		}
		if need <= available {
			return start, end, showAbove, showBelow
		}
		rows--
	}

	start, end = selectionWindow(selected, total, 1)
	return start, end, false, false
}
