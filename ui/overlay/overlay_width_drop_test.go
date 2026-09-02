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

// #3433 review. The widest row of a too-tall modal may be one of the rows the
// height clip discards. fgWidth was measured before that clip, so it stayed at the
// pre-clip value and the width branch fired on rows that were never too wide —
// padding every retained row across the whole frame and dragging a centered modal
// to column zero.
func TestPlaceOverlayRemeasuresWidthAfterDroppingRows(t *testing.T) {
	const cols, rows = 40, 4
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	narrow := strings.Repeat("m", 10)
	fgRows := []string{narrow, narrow, narrow, narrow,
		strings.Repeat("W", cols+20), // the widest row, and it is clipped away
	}
	fg := strings.Join(fgRows, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("got %d lines, want %d", len(lines), rows)
	}
	if strings.Contains(out, "W") {
		t.Fatal("precondition: the over-wide row must have been clipped away by height")
	}
	// The retained rows are 10 cells in a 40-cell frame, so the modal must still be
	// centered with background on both sides — not stretched to the full width.
	if !strings.HasPrefix(lines[0], "x") {
		t.Fatalf("the retained rows are narrow, so the modal must stay centered with background "+
			"to its left; got %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], "x") {
		t.Fatalf("and background to its right; got %q", lines[0])
	}
}

// backgroundCharIsStyled walks the composite tracking SGR state and reports
// whether any background cell ('x' here) is rendered while a non-reset style is
// still in effect. Checking only the END of the output is not enough: the
// compositor writes background after the foreground on EVERY row, so a leak shows
// up mid-frame long before it reaches the last byte.
func backgroundCharIsStyled(out string) bool {
	open := ""
	for i := 0; i < len(out); {
		if loc := sgrRegex.FindStringIndex(out[i:]); loc != nil && loc[0] == 0 {
			seq := out[i : i+loc[1]]
			if seq == "\x1b[0m" || seq == "\x1b[m" {
				open = ""
			} else {
				open = seq
			}
			i += loc[1]
			continue
		}
		if out[i] == 'x' && open != "" {
			return true
		}
		i++
	}
	return false
}

// #3433 review. Closing the style only on the LAST retained row still lets it
// color the background tail of every earlier row: the compositor writes background
// after the foreground on each one. The end-of-output check alone cannot see that.
func TestPlaceOverlayDoesNotStyleBackgroundOnEarlierRows(t *testing.T) {
	const cols, rows = 40, 6
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	fgRows := make([]string, rows+4)
	for i := range fgRows {
		fgRows[i] = "modal"
	}
	fgRows[0] = "\x1b[31mmodal"            // opens on the FIRST retained row
	fgRows[len(fgRows)-1] = "modal\x1b[0m" // resets on a row the clip discards
	fg := strings.Join(fgRows, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	if backgroundCharIsStyled(out) {
		t.Fatal("no background cell may be rendered inside the modal's style: the style must be " +
			"closed before each background segment, not merely by the end of the frame")
	}
}

// #3433 review. A hyperlink is state the terminal holds just like a color, and the
// height clip can discard the row that closes it. OSC 8 is reachable here — ui/err.go
// handles it (#565) — so an unterminated one would leave every background cell after
// the modal clickable, and outlive the frame.
func TestPlaceOverlayClosesAHyperlinkWhoseCloserWasClipped(t *testing.T) {
	const cols, rows = 40, 4
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	open := "\x1b]8;;https://example.com\x1b\\"
	closer := "\x1b]8;;\x1b\\"
	fgRows := []string{open + "link", "more", "more", "more", "more" + closer}
	fg := strings.Join(fgRows, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	if strings.Count(out, "https://example.com") != strings.Count(out, closer) {
		t.Fatalf("every opened hyperlink must be closed in the composite; opens=%d closes=%d",
			strings.Count(out, "https://example.com"), strings.Count(out, closer))
	}
}

// The other direction for the hyperlink path, so closing cannot become
// unconditional: a row that opens AND closes its own link gets nothing appended.
func TestPlaceOverlayLeavesASelfClosedHyperlinkAlone(t *testing.T) {
	const cols, rows = 40, 4
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
	fg := "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\nplain"

	out := PlaceOverlay(0, 0, fg, bg, true)

	if strings.Contains(out, "\x1b]8;;\x1b\\\x1b]8;;\x1b\\") {
		t.Fatal("no spurious hyperlink closer may be appended to a row that closed its own")
	}
}

// hasUnterminatedOSC reports whether s contains an OSC introducer with no
// terminator after it — the shape that swallows whatever the terminal is given
// next as OSC payload.
func hasUnterminatedOSC(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != 0x1b || s[i+1] != ']' {
			continue
		}
		rest := s[i+2:]
		st := strings.Index(rest, "\x1b\\")
		bel := strings.IndexByte(rest, '\a')
		if st < 0 && bel < 0 {
			return true
		}
	}
	return false
}

// #3433 review. reflow's truncator is not OSC-aware: it takes the first letter of
// a URI for the sequence terminator and counts the rest as visible text, so the
// width clip could cut a hyperlink in half and emit an unterminated OSC command.
// That is worse than a mispositioned row — it corrupts output past the modal.
func TestPlaceOverlayNeverSplitsAHyperlinkWhenClipping(t *testing.T) {
	const cols, rows = 10, 3
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	// Four visible cells, but ansi.PrintableRuneWidth reads 21 because it counts
	// the URI — so the width clip fires on a row that genuinely fits.
	link := "\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\"
	if w := ansi.PrintableRuneWidth(link); w <= cols {
		t.Fatalf("precondition: the row must read too wide under the compositor's measure; got %d", w)
	}
	fg := strings.Join([]string{link, link}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	if hasUnterminatedOSC(out) {
		t.Fatalf("the clip must not cut through an OSC sequence; got %q", out)
	}
	if !strings.Contains(out, "https://example.com") {
		t.Fatalf("an atomic sequence must survive intact, not be shortened; got %q", out)
	}
}

// #3433 review. A hyperlink escape occupies ZERO cells on every terminal — unlike
// grapheme clustering, there is no emulator disagreement to defer here, so
// measuring a row's URI as visible text is simply wrong. It left the compositor
// computing a negative remaining width for such a row: no padding, no right-hand
// background, four visible cells sitting in a ten-cell frame.
func TestPlaceOverlayMeasuresAHyperlinkRowByItsVisibleCells(t *testing.T) {
	const cols, rows = 10, 3
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	link := "\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\"
	fg := strings.Join([]string{link, link}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("got %d lines, want %d", len(lines), rows)
	}
	// The modal is 4 visible cells in a 10-cell frame, so the row must be completed
	// — by background or by padding — not left short.
	for i, line := range lines {
		if w := lineWidth(line); w != cols {
			t.Fatalf("line %d is %d visible cells, want exactly %d: %q", i, w, cols, line)
		}
	}
}

// lineWidth corrects OSC measurement WITHOUT changing the measure for anything
// else. That distinction is the whole justification for making this correction
// inside a PR that explicitly defers the measure question, so it is pinned: for
// any content without an OSC sequence — emoji, CJK, SGR, plain — lineWidth must
// agree with ansi.PrintableRuneWidth exactly. If someone later "simplifies" it to
// xansi.StringWidth, this fails, because that WOULD be the deferred change.
func TestLineWidthMatchesPrintableRuneWidthWithoutOSC(t *testing.T) {
	for _, s := range []string{
		"plain",
		"",
		"\x1b[31mred\x1b[0m",
		strings.Repeat(joinedFamily, 5),
		"你好世界",
		"mixed 你 \x1b[1mbold\x1b[0m 👍",
		strings.Repeat("x", 200),
	} {
		if got, want := lineWidth(s), ansi.PrintableRuneWidth(s); got != want {
			t.Fatalf("lineWidth(%q) = %d, ansi.PrintableRuneWidth = %d: they must be identical "+
				"for content with no OSC sequence, or this is a measure change and not a correction", s, got, want)
		}
	}
}

// #3433 review. A row carrying BOTH a hyperlink and clustered text survives an
// x/ansi clip at its cell width while still measuring far wider under the
// compositor's own measure. Capping the recorded width only hid that from the
// clamp: pos overshot, and the row went out with neither padding nor background.
func TestPlaceOverlayClipsARowMixingAHyperlinkAndClusteredText(t *testing.T) {
	const cols, rows = 10, 3
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	mixed := "\x1b]8;;https://example.com\x1b\\" + strings.Repeat(joinedFamily, 3) + "\x1b]8;;\x1b\\"
	if lineWidth(mixed) <= cols {
		t.Fatalf("precondition: the row must still read too wide after discounting the OSC; got %d", lineWidth(mixed))
	}
	fg := strings.Join([]string{mixed, mixed}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	for i, line := range strings.Split(out, "\n") {
		if w := lineWidth(line); w != cols {
			t.Fatalf("line %d is %d cells, want exactly %d: a mixed row must be clipped to the "+
				"compositor's measure, not merely recorded as if it were", i, w, cols)
		}
	}
	if hasUnterminatedOSC(out) {
		t.Fatalf("and the clip must still not split the sequence; got %q", out)
	}
}

// The 8-bit ST terminator is a valid OSC form, and the parser handles it where a
// hand-rolled pattern of this file's own kept missing forms.
func TestLineWidthDiscountsAnEightBitTerminatedHyperlink(t *testing.T) {
	link := "\x1b]8;;https://example.com\u009cLINK"
	if got := lineWidth(link); got != 4 {
		t.Fatalf("lineWidth = %d, want 4: an OSC sequence occupies no cells whichever "+
			"terminator it uses", got)
	}
}
