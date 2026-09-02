package layout

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// Cells reports how many terminal cells s occupies once rendered.
//
// This is the ONE width answer for the TUI: panes size and clamp themselves with
// it, the overlay compositor places and clips with it, and app's overlayOrigin
// positions modals and registers their mouse zones with it. Before #3585 those
// used different functions, so a pane could be certain a modal fitted while the
// compositor was certain it did not — and the zones ended up registered somewhere
// the modal was not drawn (#3433, #3585).
//
// # Why x/ansi, and what it costs
//
// af renders into tmux, so the measure has to count what tmux ADVANCES. Measured
// on tmux 3.4 against an isolated socket, cursor_x after printing each string:
//
//	case                tmux  x-ansi  runewidth  PrintableRuneWidth
//	ascii "abcd"           4     4        4            4
//	CJK                    4     4        4            4
//	emoji U+1F600          2     2        2            2
//	heart + VS16           2     2        1  X         1  X
//	heart, text form       1     1        1            1
//	ZWJ family (4+3ZWJ)    4     2  X     2  X         8  X
//	ZWJ couple             2     2        2            5  X
//	ZWJ profession         2     2        2            4  X
//	skin-tone modifier     2     2        2            4  X
//	regional-indicator     2     2        1  X         2
//	combining accent       1     1        1            1
//	keycap                 2     2        1  X         1  X
//	Hangul                 4     4        4            4
//	fullwidth latin        4     4        4            4
//
// x-ansi is exact on 13 of 14. af's own chrome — the box-drawing, arrows, bullets
// and carets the frame is built from — measures 1 for every candidate and for
// tmux, so nothing in the ordinary UI turns on this choice.
//
// The single miss is the chained ZWJ family: x-ansi says 2 where tmux advances 4.
// #3585's triage asked for an OVERESTIMATE on disagreement, reasoning that an
// underestimate writes past the frame. That was implemented first —
// max(x-ansi, PrintableRuneWidth) — and the play-test showed a cost the reasoning
// could not have predicted:
//
//	a sidebar row for a session titled "fam<family>zzz" measures 36 against a real
//	30, so ClampToRect cuts it, and what it cuts is the TAIL — where the ● status
//	dot lives. Every session whose title carries clustered emoji silently loses its
//	status marker.
//
// So the overestimate is not free: it deletes content that fits, on ordinary user
// data, in the most-looked-at pane. The underestimate's cost is a row that
// overflows and wraps (#3430), which takes many families in ONE row to reach and
// which #3578's clip bounds. Measured harm beat predicted harm, so the measure is
// x-ansi; the deviation from the triage's rule is deliberate and flagged on the PR.
//
// Escape sequences are discounted: x-ansi parses them, including OSC hyperlinks,
// whose URIs PrintableRuneWidth would otherwise count as visible text (#3433).
func Cells(s string) int {
	return xansi.StringWidth(s)
}

// BlockWidth is the widest LINE of a multi-line block — lipgloss.Width's shape,
// on Cells' measure.
//
// Cells measures one line. Handing it a whole block does not fail, it returns the
// SUM of every row, because that is what x/ansi's StringWidth does with newlines
// in it — and lipgloss.Width, which the callers replaced, takes the max instead.
// Measured: "abcd\nab\nabcdefgh" is 8 to lipgloss.Width and 14 to
// xansi.StringWidth.
//
// That distinction is not cosmetic. overlayOrigin measures a whole FRAME to
// centre a modal in it; summing its rows reported a 120-column frame as ~188,
// pushed the modal to the right-hand clamp, and left every mouse zone registered
// somewhere the modal was not drawn — the exact defect #3585 is about,
// reintroduced by the fix for it. Caught by the real-terminal play-test, which is
// why that gate exists.
func BlockWidth(s string) int {
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		if w := Cells(line); w > widest {
			widest = w
		}
	}
	return widest
}

// TruncateToCells shortens s until it fits width by the Cells measure, without
// ever cutting through a control sequence.
//
// Two measures are in play and they are not interchangeable. x/ansi's truncator
// is the one that keeps escape sequences atomic — reflow's takes the first letter
// of an OSC hyperlink's URI for the terminator and cuts through it (#3433) — but
// it truncates by ITS OWN cell count, which is not what Cells reports whenever
// Cells takes the legacy overestimate. Asking it once for `width` and trusting
// the result is therefore wrong in a way that is easy to miss: clamping "\U0001F44D\U0001F3FD"
// (Cells 4, x/ansi 2) into 3 columns returns the grapheme untouched, still 4 by
// Cells, so a caller that then pads on `Cells < width` pads nothing and emits a
// row narrower than the rectangle it promised.
//
// So the parser is asked for progressively less until the result actually fits.
// A row that already fits returns on the first attempt.
func TruncateToCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if Cells(s) <= width {
		return s
	}
	for w := width; w >= 0; w-- {
		out := xansi.Truncate(s, w, "")
		if Cells(out) <= width {
			return out
		}
	}
	return ""
}
