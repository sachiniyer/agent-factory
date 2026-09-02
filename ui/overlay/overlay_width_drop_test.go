package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/ansi"
)

// joinedFamily is one emoji ZWJ sequence: four emoji joined by three ZWJs.
//
// It is the shape the three width functions in this tree disagree about, and the
// disagreement is not small. Measured:
//
//	lipgloss.Width / xansi.StringWidth / runewidth.StringWidth -> 2
//	ansi.PrintableRuneWidth (what this compositor uses)        -> 8
//	tmux 3.4, actually advancing the cursor                    -> 4
//
// Panes size and truncate themselves with the first, so a modal built from this
// text believes it fits. PlaceOverlay measures with the third.
const joinedFamily = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"

// #3433. A modal whose content the compositor reads as wider than the terminal
// used to make PlaceOverlay `return fg` — discarding the ENTIRE background frame.
// A one-cell arithmetic disagreement blanked the whole TUI, silently, and only on
// the frames where such text happened to be on screen.
//
// Eleven families read as 88 cells under PrintableRuneWidth, over an 80-column
// background, while every cell-accurate measure puts the row at 22 and tmux at
// 44. So the modal genuinely fits and only the compositor thinks otherwise —
// which is exactly the case that must not cost the user their frame.
func TestPlaceOverlayKeepsTheFrameWhenAModalMeasuresTooWide(t *testing.T) {
	const cols, rows = 80, 24
	bgRow := strings.Repeat("x", cols)
	bg := strings.TrimSuffix(strings.Repeat(bgRow+"\n", rows), "\n")

	modalRow := strings.Repeat(joinedFamily, 11)
	if w := ansi.PrintableRuneWidth(modalRow); w <= cols {
		t.Fatalf("precondition: the modal row must read too wide under the compositor's measure; got %d for %d columns", w, cols)
	}
	fg := strings.Join([]string{modalRow, modalRow, modalRow}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("the frame must survive an over-wide modal: got %d lines, want %d (the background was dropped)", len(lines), rows)
	}
	if !strings.Contains(out, "x") {
		t.Fatal("the background must still be composited underneath, not replaced by the modal")
	}
	// The other direction: clipping must not turn into dropping the modal.
	if !strings.Contains(out, "\U0001F468") {
		t.Fatal("the modal must still be rendered (clipped), not omitted")
	}
	// And nothing may be written past the terminal's own width.
	for i, line := range lines {
		if w := ansi.PrintableRuneWidth(line); w > cols {
			t.Fatalf("line %d overflows the background: width %d > %d", i, w, cols)
		}
	}
}

// The height half of the same branch: a modal taller than the terminal must lose
// its overflowing rows, not take the frame with it.
func TestPlaceOverlayKeepsTheFrameWhenAModalIsTooTall(t *testing.T) {
	const cols, rows = 40, 6
	bgRow := strings.Repeat("x", cols)
	bg := strings.TrimSuffix(strings.Repeat(bgRow+"\n", rows), "\n")

	// Every modal row is DISTINGUISHABLE, which is what gives this teeth. Counting
	// lines alone cannot fail: the composite loop is driven by bgLines, so the
	// frame is `rows` tall either way. Without the clip, placeY clamps to a
	// NEGATIVE offset (bgHeight-fgHeight) and the frame renders the modal's BOTTOM
	// rows — the modal silently scrolled past its own title.
	modalRows := make([]string, rows+4)
	for i := range modalRows {
		modalRows[i] = strings.Repeat(string(rune('A'+i)), 10)
	}
	fg := strings.Join(modalRows, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("an over-tall modal must be clipped to the frame: got %d lines, want %d", len(lines), rows)
	}
	if !strings.Contains(lines[0], modalRows[0]) {
		t.Fatalf("the frame must show the modal's FIRST row at the top; got %q, want it to contain %q "+
			"(a negative placeY renders the modal's tail instead)", lines[0], modalRows[0])
	}
	if strings.Contains(out, modalRows[len(modalRows)-1]) {
		t.Fatalf("the modal's overflowing rows must be clipped away; %q is still on screen", modalRows[len(modalRows)-1])
	}
}

// The clamp below the clip must never see a negative upper bound. Before #3433
// the oversize cases returned early, so `clamp(placeX, 0, bgWidth-fgWidth)` was
// unreachable with fgWidth > bgWidth; clipping makes it reachable, and it is only
// safe because the clip sets fgWidth == bgWidth first. This pins that ordering.
func TestPlaceOverlayClipsBeforeClamping(t *testing.T) {
	const cols, rows = 20, 5
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
	// Oversize in BOTH dimensions at once, and centered, so a negative slack would
	// reach the clamp from both directions.
	fgRow := strings.Repeat(joinedFamily, 6) // 48 cells by the compositor's measure
	fg := strings.TrimSuffix(strings.Repeat(fgRow+"\n", rows+3), "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("got %d lines, want %d", len(lines), rows)
	}
	for i, line := range lines {
		if w := ansi.PrintableRuneWidth(line); w != cols {
			t.Fatalf("line %d width %d, want exactly %d: the frame must stay rectangular", i, w, cols)
		}
	}
}

// openSGR reports whether s ends with a styling sequence still in effect.
func openSGR(s string) bool {
	m := sgrRegex.FindAllString(s, -1)
	if len(m) == 0 {
		return false
	}
	last := m[len(m)-1]
	return last != "\x1b[0m" && last != "\x1b[m"
}

// #3433 review. Clipping rows off an over-tall modal can discard the row that
// CLOSES a style opened on a retained row. The compositor writes background cells
// after every foreground row, so an unterminated SGR bleeds the modal's color
// across the frame and onward past the end of the output.
func TestPlaceOverlayClosesAStyleWhoseResetWasClipped(t *testing.T) {
	const cols, rows = 40, 6
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	fgRows := make([]string, rows+4)
	for i := range fgRows {
		fgRows[i] = "modal"
	}
	fgRows[0] = "\x1b[31mmodal"            // opened on a RETAINED row
	fgRows[len(fgRows)-1] = "modal\x1b[0m" // closed on a row the clip discards
	fg := strings.Join(fgRows, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	if openSGR(out) {
		t.Fatal("the composite must not end with an open SGR: the clipped modal's style would " +
			"bleed across the background and past the end of the frame")
	}
}

// The width half of the same concern, verifying rather than assuming: the reflow
// truncator calls ResetAnsi() when it cuts through an SGR, so a clipped-wide row
// closes its own styling. If that ever stopped being true, the width clip would
// need the same treatment the height clip gets.
func TestPlaceOverlayClippedWideRowClosesItsOwnStyle(t *testing.T) {
	const cols, rows = 20, 4
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	// Opens a color, and its reset sits past the clip point.
	wide := "\x1b[31m" + strings.Repeat("m", cols*2) + "\x1b[0m"
	fg := strings.Join([]string{wide, wide}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	if openSGR(out) {
		t.Fatal("a row clipped for width must not leave its style open")
	}
}

// And the other direction, so closing-at-the-cut cannot become "always append a
// reset": a modal that fits is composited byte-for-byte as it arrived, keeping
// whatever styling it brought.
func TestPlaceOverlayLeavesAFittingModalsStylingAlone(t *testing.T) {
	const cols, rows = 40, 6
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
	fg := "\x1b[31mmodal\x1b[0m\nplain"

	out := PlaceOverlay(0, 0, fg, bg, true)

	if !strings.Contains(out, "\x1b[31mmodal\x1b[0m") {
		t.Fatalf("a fitting modal's styling must pass through untouched; got %q", out)
	}
	if strings.Contains(out, "\x1b[0m\x1b[0m") {
		t.Fatal("no spurious reset may be appended to a modal that was not clipped")
	}
}
