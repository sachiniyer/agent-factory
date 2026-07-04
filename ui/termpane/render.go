package termpane

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// renderGrid turns the emulator's visible cell grid into exactly height
// ANSI-styled lines of exactly width cells, padding with blanks where the
// grid is smaller and clipping where it is larger (the owner resizes the
// emulator to the pane rect, so both are transient states around a resize).
// The caller must hold the lock that guards the emulator against concurrent
// writes (TermPane.gridMu): CellAt returns pointers into the live buffer.
//
// cursorAt marks the cell the terminal cursor occupies so renderGrid can
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
func renderGrid(emu *vt.Emulator, width, height int, cursor cursorAt) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	var sb strings.Builder
	sb.Grow(width * height * 2)
	for y := 0; y < height; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		prev := uv.Style{}
		for x := 0; x < width; {
			content, cellWidth, style := " ", 1, uv.Style{}
			if c := emu.CellAt(x, y); c != nil && c.Width > 0 && c.Content != "" {
				content, cellWidth, style = c.Content, c.Width, c.Style
			}
			if x+cellWidth > width {
				// A wide glyph straddling the clip boundary (only possible
				// while the emulator is transiently larger than the pane)
				// would overflow the row: blank it instead.
				content, cellWidth = " ", 1
			}
			if cursor.show && y == cursor.y && x <= cursor.x && cursor.x < x+cellWidth {
				style.Attrs ^= uv.AttrReverse
			}
			sb.WriteString(style.Diff(&prev))
			prev = style
			sb.WriteString(content)
			x += cellWidth
		}
		if !prev.IsZero() {
			sb.WriteString("\x1b[m")
		}
	}
	return sb.String()
}
