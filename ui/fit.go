package ui

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// fitLine clips s to width terminal cells, marking the cut with an ellipsis.
//
// The measurement is ANSI-aware on purpose (#2149). A plain rune width counts
// every byte of an escape sequence as a visible cell, so styled content gets
// truncated many cells early — and, because the cut then lands at a byte
// offset, usually in the MIDDLE of an escape sequence. A terminal swallows
// everything following an unterminated CSI until a final byte arrives, so
// inside a modal that eats the row's own padding and right border and lets the
// pane behind print into the form. xansi.Truncate cuts on cell boundaries and
// never splits a sequence; the trailing reset closes any style the cut left
// open.
//
// It measures with layout.Cells, the ONE width answer (#3585/#3610), which is
// what the rail header budgets its ladder in before calling this (#3641). The
// two were lipgloss.Width and layout.Cells — the same answer for every string
// either has been handed, but "the same answer today" is how a shared invariant
// stops being one.
func fitLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if layout.Cells(s) <= width {
		return s
	}
	tail := "…"
	if width < layout.Cells(tail) {
		tail = ""
	}
	out := xansi.Truncate(s, width, tail)
	if strings.Contains(out, "\x1b") {
		out += "\x1b[0m"
	}
	return out
}

func fitBlockToSize(content string, width, height, pinnedFooter int) string {
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = fitLine(lines[i], width)
	}
	if height <= 0 || len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	if pinnedFooter < 0 {
		pinnedFooter = 0
	}
	if pinnedFooter > height {
		pinnedFooter = height
	}
	bodyRows := height - pinnedFooter
	fitted := make([]string, 0, height)
	if bodyRows > 0 {
		fitted = append(fitted, lines[:bodyRows]...)
		if len(lines) > height {
			fitted[bodyRows-1] = taskFormMoreStyle.Render(fitLine("  ↓ more", width))
		}
	}
	if pinnedFooter > 0 {
		fitted = append(fitted, lines[len(lines)-pinnedFooter:]...)
	}
	return strings.Join(fitted, "\n")
}
