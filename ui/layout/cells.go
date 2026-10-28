package layout

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/ansi"
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
//
// # One line or many
//
// Cells reports the WIDEST line of a multi-line block, not the sum of its rows.
// x/ansi's StringWidth does sum them, and that is not a defensible answer for
// anything: no caller wants "how many cells would these rows occupy end to end".
// It cost a real defect once already — overlayOrigin measures a whole FRAME to
// centre a modal in it, and the summed width reported a 120-column frame as ~188,
// pushed the modal to the right-hand clamp, and left every mouse zone registered
// somewhere the modal was not drawn, which is #3585's own defect reintroduced by
// its fix. It was caught by the real-terminal play-test and repaired at the one
// call site; taking the widest line here means no caller can reach the summed
// answer again (#3614). BlockWidth is kept as the explicit spelling of the same
// thing for callers that replaced lipgloss.Width and want to say so.
func Cells(s string) int {
	if !strings.ContainsRune(s, '\n') {
		return xansi.StringWidth(s)
	}
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		if w := xansi.StringWidth(line); w > widest {
			widest = w
		}
	}
	return widest
}

// CellsUpperBound reports a width the string is guaranteed NOT to exceed in tmux.
//
// It is the max rule #3610 measured and then withdrew as the general measure: on
// the corpus in Cells' table it never under-reports what tmux advances, where
// each half alone does — x/ansi under-reports the chained ZWJ family (2 against
// 4), and PrintableRuneWidth under-reports a variation-selector emoji (1 against
// 2). It is NOT a better measure than Cells; it is a deliberate OVERestimate,
// wrong by 4 cells on that same family, which is exactly why adopting it
// everywhere cut the ● status dot off ordinary sidebar rows.
//
// So it has one job: bound a row whose width cannot be known, in the one
// direction the fixed-rectangle contract can survive. See the contract measure in
// contract.go for where that applies and why under-filling is the accepted cost.
//
// Multi-line input takes the widest line, like Cells. The stripped string is what
// PrintableRuneWidth is fed, because it counts an OSC hyperlink's URI as visible
// text (#3433) and would otherwise report a bound larger than any rectangle.
func CellsUpperBound(s string) int {
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		w := xansi.StringWidth(line)
		if p := ansi.PrintableRuneWidth(xansi.Strip(line)); p > w {
			w = p
		}
		if w > widest {
			widest = w
		}
	}
	return widest
}

// BlockWidth is the widest LINE of a multi-line block — lipgloss.Width's shape,
// on Cells' measure. It is now exactly Cells, and kept as the explicit spelling
// for callers that replaced lipgloss.Width and want the code to say which shape
// they meant.
//
// It was not always the same function. Cells summed a block's rows until #3614,
// because that is what x/ansi's StringWidth does with newlines in it, and
// BlockWidth existed to stop callers reaching that answer. Cells takes the widest
// line itself now, so the trap is closed at the source rather than at the one
// call site that fell into it; see Cells.
func BlockWidth(s string) int { return Cells(s) }

// TruncateToCells shortens s until it fits width by the Cells measure, without
// ever cutting through a control sequence.
//
// x/ansi's truncator is the one that keeps escape sequences atomic — reflow's
// takes the first letter of an OSC hyperlink's URI for the terminator and cuts
// through it (#3433) — and it truncates by its OWN cell count. That count is
// Cells today, so one call would do; the loop stays because the two are not the
// same function and nothing forces them to agree. Asking once and trusting the
// result fails in a way that is easy to miss: if the truncator ever counts a
// grapheme lower than the measure the caller pads by, it returns a row that still
// measures over, the pad is skipped on `Cells < width`, and the row goes out
// narrower than the rectangle it promised.
//
// So the parser is asked for progressively less until the result actually fits.
// A row that already fits returns on the first attempt, which is every ordinary
// row. truncateToContract in contract.go is the same loop against the contract
// measure, where the two genuinely do disagree.
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
