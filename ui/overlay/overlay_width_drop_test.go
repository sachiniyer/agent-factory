package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/ansi"

	"github.com/sachiniyer/agent-factory/ui/layout"
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

	// Under the OLD split measures this row read 88 cells against an 80-column
	// frame and tripped the drop. Since #3585 the compositor measures with
	// layout.Cells, which puts it at 22 — so it now correctly FITS, and the false
	// positive that started #3433 cannot arise. The frame assertions below still
	// hold, and the genuinely-too-wide case is covered by the sibling test.
	modalRow := strings.Repeat(joinedFamily, 11)
	if w := ansi.PrintableRuneWidth(modalRow); w <= cols {
		t.Fatalf("precondition: this row is what the OLD measure over-read; got %d", w)
	}
	if w := lineWidth(modalRow); w > cols {
		t.Fatalf("precondition: under the unified measure it must fit; got %d for %d columns", w, cols)
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
	// And nothing may be written past the terminal's own width, by the measure the
	// compositor actually uses.
	for i, line := range lines {
		if w := lineWidth(line); w > cols {
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
	fgRow := strings.Repeat(joinedFamily, 30) // 60 cells by the compositor's measure, over a 20-cell frame
	fg := strings.TrimSuffix(strings.Repeat(fgRow+"\n", rows+3), "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	lines := strings.Split(out, "\n")

	if len(lines) != rows {
		t.Fatalf("got %d lines, want %d", len(lines), rows)
	}
	for i, line := range lines {
		if w := lineWidth(line); w != cols {
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
	// The retained rows are 10 cells in a 40-cell frame, so the modal must still
	// be centered with blank margins on both sides (#5297: the cells beside a
	// modal are blank backdrop, not background) — not stretched to the full
	// width.
	if want := strings.Repeat(" ", 15) + narrow + strings.Repeat(" ", 15); lines[0] != want {
		t.Fatalf("the retained rows are narrow, so the modal must stay centered with blank "+
			"margins on both sides; got %q, want %q", lines[0], want)
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

// lineWidth is layout.Cells — the compositor shares the ONE measure with the panes,
// app's overlayOrigin and the mouse zones (#3585). Pinned, because the whole point
// of that issue is that these cannot be allowed to drift apart again.
//
// This replaces a guard that required lineWidth to equal ansi.PrintableRuneWidth for
// non-OSC content. That guard existed to stop #3433's OSC correction from silently
// becoming the deferred measure change; #3585 MAKES that change, deliberately and
// from tmux measurements, so the guard's job has moved to layout's corpus test.
func TestLineWidthIsTheSharedMeasure(t *testing.T) {
	for _, s := range []string{
		"plain", "", "\x1b[31mred\x1b[0m", strings.Repeat(joinedFamily, 5),
		"你好世界", "mixed 你 \x1b[1mbold\x1b[0m 👍", strings.Repeat("x", 200),
		"\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\",
	} {
		if got, want := lineWidth(s), layout.Cells(s); got != want {
			t.Fatalf("lineWidth(%q) = %d, layout.Cells = %d: the compositor must not have its own measure", s, got, want)
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

	mixed := "\x1b]8;;https://example.com\x1b\\" + strings.Repeat(joinedFamily, 12) + "\x1b]8;;\x1b\\"
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

// #3433 review. The routing predicate this replaces compared the two measures and
// treated equality as "safe for the reflow truncator". Unsound: the OSC overcount
// can CANCEL the grapheme disagreement. Codex's example — one ASCII char, a
// hyperlink, and a joined family — measures 9 under BOTH expressions, so it took
// the reflow branch and came out as an unterminated OSC command.
func TestPlaceOverlayClipsARowWhoseMeasuresCoincidentallyAgree(t *testing.T) {
	const cols, rows = 8, 3
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")

	row := "a\x1b]8;;https://x\x1b\\" + joinedFamily
	if lineWidth(row) != ansi.PrintableRuneWidth(row) {
		t.Skipf("this case needs the two measures to coincide; got %d and %d",
			lineWidth(row), ansi.PrintableRuneWidth(row))
	}
	fg := strings.Join([]string{row, row}, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)
	if hasUnterminatedOSC(out) {
		t.Fatalf("a row whose measures happen to agree still must not be cut through; got %q", out)
	}
	for i, line := range strings.Split(out, "\n") {
		if w := lineWidth(line); w != cols {
			t.Fatalf("line %d is %d cells, want exactly %d", i, w, cols)
		}
	}
}

// The hyperlink detector's coverage must not drift from the parser's. A terminator
// the parser discounts in the width but this scan fails to recognise is a link
// the compositor never closes — which is how the C1 ST form was missed, and how
// the bare-ESC form (any ESC ends an OSC string) was missed after it (#7560).
func TestOSC8DetectionCoversEveryFormTheParserDiscounts(t *testing.T) {
	for _, form := range []struct{ name, opener string }{
		{"7-bit ST", "\x1b]8;;https://example.com\x1b\\"},
		{"BEL", "\x1b]8;;https://example.com\a"},
		{"C1 ST", "\x1b]8;;https://example.com\u009c"},
		// Any ESC terminates an OSC string, so the bare-ESC form is the fourth
		// terminator the parser discounts — the one #7560 missed. It ends with a
		// CSI introducer so the row still measures four visible cells.
		{"bare ESC", "\x1b]8;;https://example.com\x1b[31m"},
	} {
		row := form.opener + "LINK"
		// The parser discounts it: the row is four visible cells.
		if got := lineWidth(row); got != 4 {
			t.Fatalf("%s: lineWidth = %d, want 4 (precondition: the parser discounts this form)", form.name, got)
		}
		// So the detector must see the open link and emit its OSC 8 closer.
		// Asserting the closer byte-for-byte, not just "non-empty", because the
		// bare-ESC row above also carries an SGR whose own reset would make a
		// bare non-empty check pass without the link ever being closed.
		got := closeSequences(row)
		if !strings.Contains(got, "\x1b]8;;\x1b\\") {
			t.Fatalf("%s: an opened hyperlink was not detected, so the compositor would never close it; "+
				"closeSequences(%q) = %q", form.name, row, got)
		}
	}
}

// The pinned coverage test above is the unit-level assertion; this is the
// end-to-end one for the bare-ESC form that pin used to miss. A bare-ESC-
// terminated OSC 8 opener flows into PlaceOverlay as the modal's foreground —
// the path an agent-supplied attention reason or task name takes into the
// alarm banner / sidebar — and before the fix the scan did not see the form,
// so closeSequences emitted no closer and the composite carried the opener
// alone. A real terminal (tmux 3.5a opens the link at the bare ESC) then held
// the link open across the rest of the row, every following row, and each
// subsequent frame until an explicit OSC 8 closer arrived — the whole UI
// surface clickable to the attacker's URI. Every URI the composite emits must
// be paired with a closer.
func TestPlaceOverlayClosesABareEscTerminatedHyperlinkAcrossTheFrame(t *testing.T) {
	const cols, rows = 40, 4
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
	// The bare-ESC opener followed by a CSI: the spelling that measures the same
	// as the ST form (both are discounted to the four visible cells "LINK") AND
	// the spelling the exploit needs (tmux opens the link at the bare ESC,
	// \x1b[0m does not clear it). "second" makes the foreground wider than the
	// hyperlink row so the modal is genuinely composited, not the whole frame.
	fg := "\x1b]8;;https://evil.example\x1b[31mLINK\nsecond"

	out := PlaceOverlay(0, 0, fg, bg, false)

	const closer = "\x1b]8;;\x1b\\"
	opens, closes := strings.Count(out, "https://evil.example"), strings.Count(out, closer)
	if opens != closes {
		t.Fatalf("every opened hyperlink must be closed in the composite; opens=%d closes=%d "+
			"(the bare-ESC form used to slip past the scan, leaving the opener with no closer); got %q",
			opens, closes, out)
	}
	if opens == 0 {
		t.Fatalf("precondition: the opener must survive in the composite; got %q", out)
	}
	// And the terminal would see no unterminated OSC: a scanner looking for an
	// OSC introducer with no ST/BEL after it must find none, since the closer
	// supplies the ST.
	if hasUnterminatedOSC(out) {
		t.Fatalf("the composite must not leave an OSC introducer unterminated; got %q", out)
	}
}

// The bare-ESC form used to be invisible to the scan, so a pane preview that
// opened such a link on a row ABOVE the modal left the bgOsc8Open carry empty —
// the modal band then emitted no OSC 8 guard before its blank margin, and the
// link stayed active across the modal cells. The scanner makes the carry
// non-empty, so the modal row must open with the guard closer, exactly as it
// does for the ST form in TestPlaceOverlayClosesBackdropStateOnModalRows — and
// the reopen below the band must be the NORMALISED ST form, never the
// dangling-ESC spelling the backdrop carried in.
func TestPlaceOverlayEmitsBackdropGuardForABareEscHyperlink(t *testing.T) {
	// The pane preview opens a bare-ESC link on row 0 (no closer on that row),
	// the modal occupies row 1, and row 2 is plain content the reopen path
	// re-emits the carried opener before. Only the OSC 8 introducer is on row 0
	// (no SGR), so the modal row opens with the OSC guard alone — no leading
	// SGR reset — which is what lets the assertion read the guard as a prefix.
	bg := "\x1b]8;;https://evil.example\x1bAAAA\nBBBB\nCCCC"
	out := strings.Split(PlaceOverlay(0, 1, "MM", bg, false), "\n")
	if len(out) != 3 {
		t.Fatalf("expected 3 rows, got %d: %q", len(out), out)
	}
	const guard = "\x1b]8;;\x1b\\"
	if !strings.HasPrefix(out[1], guard) {
		t.Fatalf("the modal row must open with the OSC 8 guard so the bare-ESC link opened above "+
			"does not cover the modal cells; got %q, want prefix %q", out[1], guard)
	}
	const normalized = "\x1b]8;;https://evil.example\x1b\\"
	if !strings.HasPrefix(out[2], normalized) {
		t.Fatalf("the row below the band must reopen with the NORMALISED ST form, not the "+
			"dangling-ESC spelling; got %q, want prefix %q", out[2], normalized)
	}
}

// A bare ESC that terminates one OSC 8 sequence and simultaneously introduces
// the next (as in "\x1b]8;;\x1b]8;;uri\x1b\\") is a closer followed by an opener
// for a real terminal, but Go's non-overlapping FindAllStringSubmatch consumed
// the shared ESC as the first match's terminator and resumed after it, so the
// second opener was missed and the link stayed open across the frame (Codex
// review on #7569). The scanner reconsiders the terminating ESC as the next
// sequence's introducer, so the second opener is seen and closed.
func TestCloseSequencesSeesOpenerAfterSharedEsc(t *testing.T) {
	// First sequence is a closer (empty URI), terminated by a bare ESC that also
	// opens the second sequence (uri), which an ST then terminates — an opener.
	row := "\x1b]8;;\x1b]8;;https://evil.example\x1b\\"
	got := closeSequences(row)
	if !strings.Contains(got, "\x1b]8;;\x1b\\") {
		t.Fatalf("a shared-ESC opener must be detected so its link is closed; "+
			"closeSequences(%q) = %q", row, got)
	}
}

// A malformed OSC 8 whose parameter field runs into an ESC (as in
// "\x1b]8;foo\x1b[31m;FAKE \x1b[0m") is ended by the terminal at that ESC and
// never opens a hyperlink, but a parameter field that crossed the ESC would let
// a later ESC be treated as the terminator — manufacturing an opener the
// terminal never made, replayed across rows (Codex review on #7569). The
// parameter field stops at every byte that ends an OSC, so no opener is
// detected, no closer emitted, and nothing carried.
func TestCloseSequencesIgnoresMalformedParamFieldEndedByEsc(t *testing.T) {
	row := "\x1b]8;foo\x1b[31m;FAKE \x1b[0m"
	if got := closeSequences(row); strings.Contains(got, "\x1b]8;;\x1b\\") {
		t.Fatalf("a malformed OSC ended in its parameter field must not be read as "+
			"an opener; closeSequences(%q) = %q", row, got)
	}
	if _, carry := backdropOpen(row, nil, ""); carry != "" {
		t.Fatalf("the same malformed sequence must never be carried for re-emission; got %q", carry)
	}
}

// CAN (0x18) and SUB (0x1A) don't terminate an OSC — they CANCEL it. A pane row
// like "\x1b]8;foo\x18bar;https://evil\x1b[0m" is read by a terminal as an
// aborted command followed by plain text and an SGR: no link ever opens. A
// scanner that crossed the cancellation byte would treat the later ESC as the
// terminator and manufacture the attacker-chosen link — worse than detecting
// nothing, because backdropOpen would then REPLAY it below a modal as a real
// opener the original stream never contained (Codex review on #7569, fourth
// finding). Both bytes must end the sequence in both fields.
func TestCancelledOsc8IsNotReadAsAnOpener(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"CAN in parameter field", "\x1b]8;foo\x18bar;https://evil.example \x1b[0m"},
		{"SUB in parameter field", "\x1b]8;foo\x1abar;https://evil.example \x1b[0m"},
		{"CAN in URI field", "\x1b]8;;https://evil.example\x18 \x1b[0m"},
		{"SUB in URI field", "\x1b]8;;https://evil.example\x1a \x1b[0m"},
		// With a real terminator AFTER the cancelled content, a field class
		// that crosses CAN/SUB manufactures the link the terminal aborted —
		// closer emitted now, opener replayed below any modal.
		{"CAN then ST", "\x1b]8;foo\x18bar;https://evil.example\x1b\\"},
		{"SUB then BEL", "\x1b]8;;https://evil.example\x1a\a"},
	} {
		if got := closeSequences(tc.row); strings.Contains(got, "\x1b]8;;\x1b\\") {
			t.Fatalf("%s: a cancelled OSC must not be read as an opener; "+
				"closeSequences(%q) = %q", tc.name, tc.row, got)
		}
		if _, carry := backdropOpen(tc.row, nil, ""); carry != "" {
			t.Fatalf("%s: a cancelled OSC must never be carried for re-emission; got %q",
				tc.name, carry)
		}
	}
}

// A cancellation byte ends only the sequence it lands in: output after it is
// read normally, so a VALID opener later in the row is still detected.
func TestScanContinuesAfterACancelledSequence(t *testing.T) {
	row := "\x1b]8;;cancelled\x18" + "text" + "\x1b]8;;https://real.example\a"
	got := closeSequences(row)
	if !strings.Contains(got, "\x1b]8;;\x1b\\") {
		t.Fatalf("a real opener after a cancelled sequence must still be detected; "+
			"closeSequences(%q) = %q", row, got)
	}
}

// The carry bgOsc8Open is re-emitted verbatim below a modal, so it must end in
// a real terminator. A bare-ESC match ends in a dangling ESC a terminal would
// dispatch as the next escape's start — eating the first byte of the row it
// precedes (Codex review on #7569). Normalisation appends the backslash that
// turns it into ST rather than rebuilding from the URI, so a parameter field
// like id= — the identifier associating cells with their link — survives the
// trip, where an "\x1b]8;;<uri>\x1b\\" rebuild would silently drop it.
func TestBackdropOpenNormalisesBareEscCarryToSt(t *testing.T) {
	_, carry := backdropOpen("\x1b]8;;https://evil.example\x1bAAAA", nil, "")
	const want = "\x1b]8;;https://evil.example\x1b\\"
	if carry != want {
		t.Fatalf("a bare-ESC opener must be carried as its ST form so the dangling "+
			"ESC doesn't eat the next character on re-emit; got %q, want %q",
			carry, want)
	}
}

// The parameter field, not just the URI, must survive normalisation: an opener
// carried with "id=7" re-emitted without it would associate the reopened cells
// with no link at all on terminals that honour the identifier.
func TestBackdropOpenPreservesParametersInNormalisedCarry(t *testing.T) {
	_, carry := backdropOpen("\x1b]8;id=7;https://evil.example\x1bAAAA", nil, "")
	const want = "\x1b]8;id=7;https://evil.example\x1b\\"
	if carry != want {
		t.Fatalf("the normalised carry must keep the parameter field, not rebuild "+
			"from the URI alone; got %q, want %q", carry, want)
	}
}

// scanOSC8 against the terminal-state-machine truth for each sequence shape:
// every terminator ends the string (and a bare ESC is reconsidered as the next
// introducer), CAN/SUB cancel it, and an incomplete sequence — no terminator,
// or no ';' before the terminator — is no sequence at all.
func TestScanOSC8ReadsLikeATerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
		want []string // the URI field of each sequence found, in order
	}{
		{"ST form", "\x1b]8;;https://a\x1b\\", []string{"https://a"}},
		{"BEL form", "\x1b]8;;https://a\a", []string{"https://a"}},
		{"C1 ST form", "\x1b]8;;https://a\u009c", []string{"https://a"}},
		{"bare ESC form", "x\x1b]8;;https://a\x1b[0m", []string{"https://a"}},
		{"closer", "\x1b]8;;\x1b\\", []string{""}},
		{"shared ESC closer then opener", "\x1b]8;;\x1b]8;;https://a\x1b\\",
			[]string{"", "https://a"}},
		{"opener then closer", "\x1b]8;;https://a\x07\x1b]8;;\x1b\\",
			[]string{"https://a", ""}},
		{"parameters preserved", "\x1b]8;id=9;https://a\x1b\\", []string{"https://a"}},
		{"URI may contain ';'", "\x1b]8;;https://a;b\x1b\\", []string{"https://a;b"}},
		{"parameters ended by ESC", "\x1b]8;foo\x1b[31m;FAKE\x1b[0m", nil},
		{"CAN in parameters", "\x1b]8;foo\x18bar;https://a\x1b\\", nil},
		{"SUB in parameters", "\x1b]8;foo\x1abar;https://a\x1b\\", nil},
		{"CAN in URI", "\x1b]8;;https://a\x18\x1b\\", nil},
		{"SUB in URI", "\x1b]8;;https://a\x1a\x1b\\", nil},
		{"unterminated", "\x1b]8;;https://a", nil},
		{"unterminated parameters", "\x1b]8;foo", nil},
		{"no URI separator", "\x1b]8;foo\x1b\\", nil},
		{"opener after a cancelled sequence", "\x1b]8;;dead\x18\x1b]8;;https://a\x07",
			[]string{"https://a"}},
		{"not OSC 8", "\x1b]52;c;aGk=\x1b\\", nil},
		{"plain text", "no escapes here", nil},
	} {
		var got []string
		for _, s := range scanOSC8(tc.row) {
			got = append(got, s.uri)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: scanOSC8(%q) found %d sequences %v, want %d %v",
				tc.name, tc.row, len(got), got, len(tc.want), tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: scanOSC8(%q)[%d].uri = %q, want %q",
					tc.name, tc.row, i, got[i], tc.want[i])
			}
		}
	}
}

// The carried raw sequence must end in the same terminator class the terminal
// saw — and only a bare ESC needs the appended backslash that turns it into ST;
// ST/BEL/C1 ST forms are already terminated and carry verbatim.
func TestScanOSC8RawSpansEndInTheirTerminator(t *testing.T) {
	for _, tc := range []struct{ name, row, raw string }{
		{"ST", "\x1b]8;;u\x1b\\x", "\x1b]8;;u\x1b\\"},
		{"BEL", "\x1b]8;;u\ax", "\x1b]8;;u\a"},
		{"C1 ST", "\x1b]8;;u\u009cx", "\x1b]8;;u\u009c"},
		{"bare ESC", "\x1b]8;;u\x1bx", "\x1b]8;;u\x1b"},
	} {
		seqs := scanOSC8(tc.row)
		if len(seqs) != 1 || seqs[0].raw != tc.raw {
			t.Fatalf("%s: scanOSC8(%q) raw = %+v, want one sequence ending %q",
				tc.name, tc.row, seqs, tc.raw)
		}
	}
}
