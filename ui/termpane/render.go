package termpane

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// renderGridWindow turns the emulator's visible cell grid into exactly height
// ANSI-styled lines of exactly width cells, padding with blanks where the
// grid is smaller and clipping where it is larger (the owner resizes the
// emulator to the pane rect, so both are transient states around a resize).
// A clipped row loses cells at the boundary — it carries "…" in its last
// cell so the cut is visible rather than silent (#4175).
// The caller must hold the lock that guards the emulator against concurrent
// writes (TermPane.gridMu): CellAt returns pointers into the live buffer.
//
// cursorAt marks the cell the terminal cursor occupies so renderGridWindow can
// overlay it; cursorNone renders without an overlay.
type cursorAt struct {
	x, y int
	show bool
}

var cursorNone = cursorAt{}

// Style.Diff keeps the escape output minimal — the same technique as
// vt.Render(); the row loop is re-implemented here so the pane can guarantee
// its own width x height contract regardless of the emulator's current size,
// and so the cursor cell can be overlaid (interactive mode, #1089 PR 2): the
// cursor's cell gets its reverse-video attribute flipped, which reads as a
// block cursor over any content and any color scheme. Each styled line ends
// with a reset so the styles can never bleed into the host TUI's surrounding
// chrome.
func renderGridWindow(emu *vt.Emulator, width, height, sourceY int, cursor cursorAt) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	var sb strings.Builder
	sb.Grow(width * height * 2)
	for y := 0; y < height; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		row := y + sourceY
		// A grid wider than the render rect (the transient state before
		// Resize catches up, or a pane resized behind the pane's back) drops
		// cells at the clip boundary — mark the cut with "…" in the last
		// cell rather than hard-cutting the row (#4175).
		clipped := rowClipped(emu, row, width)
		limit := width
		if clipped {
			limit--
		}
		prev := uv.Style{}
		for x := 0; x < limit; {
			content, cellWidth, style := " ", 1, uv.Style{}
			if c := emu.CellAt(x, row); c != nil && c.Width > 0 && c.Content != "" {
				content, cellWidth, style = c.Content, c.Width, c.Style
			}
			if x+cellWidth > limit {
				// A wide glyph straddling the clip boundary (only possible
				// while the emulator is transiently larger than the pane)
				// would overflow the row: blank it instead.
				content, cellWidth = " ", 1
			}
			if cursor.show && row == cursor.y && x <= cursor.x && cursor.x < x+cellWidth {
				style.Attrs ^= uv.AttrReverse
			}
			sb.WriteString(style.Diff(&prev))
			prev = style
			sb.WriteString(content)
			x += cellWidth
		}
		if clipped && !prev.IsZero() {
			// Close the row's style before the marker, so it renders as
			// chrome rather than in the cut row's last style.
			sb.WriteString("\x1b[m")
			prev = uv.Style{}
		}
		if clipped {
			sb.WriteString("…")
		}
		if !prev.IsZero() {
			sb.WriteString("\x1b[m")
		}
	}
	return sb.String()
}

// rowClipped reports whether rendering row at width would drop ink: a
// non-blank cell at x >= width, or a wide glyph whose head is inside the row
// but whose tail crosses the boundary. Blank and placeholder cells beyond the
// edge carry nothing, so a row that merely extends is not marked — matching
// the capture path, where trailing blanks are trimmed before the cut check.
func rowClipped(emu *vt.Emulator, row, width int) bool {
	// A wide glyph straddling the boundary loses its tail even when the cell
	// at width-1 reads as a blank placeholder — walk back over placeholders
	// (Width 0) to the head cell and measure its span.
	for x := width - 1; x >= 0; x-- {
		c := emu.CellAt(x, row)
		if c == nil {
			break
		}
		if c.Width == 0 {
			continue
		}
		if x+c.Width > width {
			return true
		}
		break
	}
	for x, bound := width, emu.Bounds().Max.X; x < bound; x++ {
		if c := emu.CellAt(x, row); c != nil && c.Width > 0 && strings.TrimSpace(c.Content) != "" {
			return true
		}
	}
	return false
}

func padRenderedRows(grid string, width, rows int) string {
	if width <= 0 || rows <= 0 {
		return grid
	}
	blank := strings.Repeat(" ", width)
	var sb strings.Builder
	sb.Grow(len(grid) + rows*(width+1))
	sb.WriteString(grid)
	wrote := grid != ""
	for range rows {
		if wrote {
			sb.WriteByte('\n')
		}
		sb.WriteString(blank)
		wrote = true
	}
	return sb.String()
}
