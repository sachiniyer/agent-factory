package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
)

func fitStyledLine(s string, width int) string {
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

func fitBlockToSize(content string, width, height, pinnedFooter int) string {
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = fitStyledLine(lines[i], width)
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
