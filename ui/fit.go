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
	return renderFitPlan(lines, fitBlockPlan(len(lines), height, pinnedFooter), width)
}

// moreLine is the plan entry for the "↓ more" cue a fitted block draws in place
// of its last body row when it overflows.
const moreLine = -1

// fitBlockPlan is fitBlockToSize's line plan for a block of n lines: for each
// line the fitted block draws, the index of the source line it shows, or
// moreLine for the overflow cue. The text and the row geometry a click zone
// needs both come from this one plan, so a zone cannot land on a line the
// frame did not draw there (#7175).
func fitBlockPlan(n, height, pinnedFooter int) []int {
	if height <= 0 || n <= height {
		return linesThrough(0, n)
	}
	pinnedFooter = max(0, min(pinnedFooter, height))
	bodyRows := height - pinnedFooter
	plan := linesThrough(0, bodyRows)
	if bodyRows > 0 {
		plan[bodyRows-1] = moreLine
	}
	return append(plan, linesThrough(n-pinnedFooter, n)...)
}

// linesThrough is the source line indices [from, to).
func linesThrough(from, to int) []int {
	plan := make([]int, 0, max(0, to-from))
	for i := from; i < to; i++ {
		plan = append(plan, i)
	}
	return plan
}

// renderFitPlan draws a plan over its source lines, each clipped to width.
func renderFitPlan(lines []string, plan []int, width int) string {
	fitted := make([]string, len(plan))
	for i, src := range plan {
		if src == moreLine {
			fitted[i] = taskFormMoreStyle.Render(fitLine("  ↓ more", width))
			continue
		}
		fitted[i] = fitLine(lines[src], width)
	}
	return strings.Join(fitted, "\n")
}

// planRows maps a fitted plan back to the item each drawn line shows, given
// the item each SOURCE line belongs to (-1 for chrome): what a pane reports as
// its clickable rows.
func planRows(plan, sourceRows []int) []int {
	rows := make([]int, len(plan))
	for i, src := range plan {
		rows[i] = -1
		if src >= 0 && src < len(sourceRows) {
			rows[i] = sourceRows[src]
		}
	}
	return rows
}
