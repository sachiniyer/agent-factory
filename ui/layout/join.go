package layout

import "strings"

// JoinVertical and JoinHorizontal assemble rect-sized blocks into a frame without
// re-measuring their rows against a different width function.
//
// They exist because the clamp alone does not survive the frame. lipgloss's
// JoinVertical/JoinHorizontal pad every line out to the widest line of its block,
// by lipgloss.Width — which is exactly the measure that under-counts the content
// ClampToRect deliberately under-fills for. Measured, on the rail block a clamped
// sidebar produces:
//
//	before the join   " <family>bbbbbbbbbbbbb"        x/ansi 16, upper bound 22
//	lipgloss.JoinVertical adds 6 blanks to reach 22 by ITS measure
//	after the join    " <family>bbbbbbbbbbbbb      "  x/ansi 22, upper bound 28
//
// The row leaves the clamp inside the rectangle and arrives at the terminal 2
// cells past it — the identical overflow #3614 is about, restored one layer up.
// So the whole of the fix would have been a no-op in the real app: the clamp is
// necessary, and honest assembly is what makes it sufficient.
//
// There is nothing for these to do that the panes have not already done. Every
// block handed to them is exactly Rect-sized by the §2.6 contract, so padding is
// a no-op on a well-formed frame; the padding below is for the blocks that are
// not (a divider, an empty-workspace placeholder) and it pads in the contract
// measure, which cannot overflow. For plain content — every frame af has drawn
// until a clustered title appears in one — the output is byte-identical to
// lipgloss's, which is pinned.
//
// Alignment is not a parameter: rect-sized blocks are already exact, so the only
// meaningful alignment is Left/Top and any other would be a bug in the caller.

// JoinVertical stacks blocks top to bottom, left-aligned, padding every line out
// to the widest line across all of them in the contract measure.
func JoinVertical(blocks ...string) string {
	var lines []string
	width := 0
	for _, b := range blocks {
		for _, line := range strings.Split(b, "\n") {
			lines = append(lines, line)
			if w := contractCells(line); w > width {
				width = w
			}
		}
	}
	for i, line := range lines {
		lines[i] = padToContract(line, width)
	}
	return strings.Join(lines, "\n")
}

// JoinHorizontal places blocks side by side, top-aligned. Each block is padded to
// its own widest line in the contract measure, and a block shorter than the
// tallest is filled out with blank rows of that width, so every column starts at
// the same screen coordinate on every row.
func JoinHorizontal(blocks ...string) string {
	if len(blocks) == 0 {
		return ""
	}
	cols := make([][]string, len(blocks))
	widths := make([]int, len(blocks))
	height := 0
	for i, b := range blocks {
		cols[i] = strings.Split(b, "\n")
		for _, line := range cols[i] {
			if w := contractCells(line); w > widths[i] {
				widths[i] = w
			}
		}
		if len(cols[i]) > height {
			height = len(cols[i])
		}
	}
	rows := make([]string, height)
	var b strings.Builder
	for y := 0; y < height; y++ {
		b.Reset()
		for i, col := range cols {
			line := ""
			if y < len(col) {
				line = col[y]
			}
			// Every column is padded, the last one included, so the output is
			// byte-identical to lipgloss's for plain content and a frame keeps
			// the rectangular shape the rest of the tree measures it by.
			b.WriteString(padToContract(line, widths[i]))
		}
		rows[y] = b.String()
	}
	return strings.Join(rows, "\n")
}
