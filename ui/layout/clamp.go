package layout

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
)

// ClampToRect pads and truncates s to exactly r.W×r.H terminal cells: the
// result is exactly r.H lines, each exactly r.W printable columns. This is
// the shared enforcement helper for the Pane contract "View() is exactly
// Rect-sized" (RFC §2.6), replacing the per-pane ad-hoc clamps.
//
// Truncation keeps the first r.H lines and the leading r.W columns of each
// line — a View renders top-down, so overflow is always trailing chrome;
// callers with keep-newest semantics (scrollback) trim before clamping.
// Width handling is ANSI- and wide-rune-aware: escape sequences are
// preserved and measured at zero width, and a truncated styled line gets a
// reset appended so leaked styles cannot bleed into the padding or the
// neighboring region. Only r's size is used; its position is ignored.
//
// An empty rect clamps everything to "".
func ClampToRect(s string, r Rect) string {
	if r.Empty() {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > r.H {
		lines = lines[:r.H]
	}
	out := make([]string, r.H)
	for i := range out {
		var line string
		if i < len(lines) {
			line = lines[i]
		}
		width := lipgloss.Width(line)
		if width > r.W {
			line = xansi.Truncate(line, r.W, "")
			if strings.Contains(line, "\x1b") {
				line += "\x1b[0m"
			}
			width = lipgloss.Width(line)
		}
		if width < r.W {
			line += strings.Repeat(" ", r.W-width)
		}
		out[i] = line
	}
	return strings.Join(out, "\n")
}
