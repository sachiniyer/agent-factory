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
// # Why this and not one of the library functions
//
// af renders into tmux, so the measure has to count what tmux ADVANCES. Measured
// on tmux 3.4 against an isolated socket, cursor_x after printing each string:
//
//	case                tmux  lipgloss/x-ansi  runewidth  PrintableRuneWidth
//	ascii "abcd"           4        4              4            4
//	CJK "..."              4        4              4            4
//	emoji U+1F600          2        2              2            2
//	heart + VS16           2        2              1  X         1  X
//	heart, text form       1        1              1            1
//	ZWJ family (4+3ZWJ)    4        2  X           2  X         8  X
//	ZWJ couple             2        2              2            5  X
//	ZWJ profession         2        2              2            4  X
//	skin-tone modifier     2        2              2            4  X
//	regional-indicator     2        2              1  X         2
//	combining accent       1        1              1            1
//	keycap                 2        2              1  X         1  X
//	Hangul                 4        4              4            4
//	fullwidth latin        4        4              4            4
//
// x-ansi is closest — 13 of 14 exact — and its single miss is the chained ZWJ
// family, where it says 2 and tmux advances 4. That miss is in the DANGEROUS
// direction: a row measured narrower than it renders overflows the frame and
// wraps, and a wrapped row makes every height budget above it a lie (#3430).
// PrintableRuneWidth misses in both directions.
//
// So the helper takes the LARGER of the two, which on this corpus never
// underestimates what tmux advances. That is the rule #3585's triage asked for:
// with clipping in place since #3578, an overestimate is visible and bounded —
// a modal loses a few cells — while an underestimate corrupts the frame.
//
// The cost is real and worth stating: a modal dense with chained ZWJ families is
// measured up to 4 cells per family wider than tmux draws it, so it can be
// clipped earlier than strictly necessary. That is the trade, taken deliberately.
//
// Escape sequences are discounted either way: x-ansi parses them, and
// PrintableRuneWidth is fed the stripped string because it otherwise counts an OSC
// hyperlink's URI as visible text (#3433).
func Cells(s string) int {
	width := xansi.StringWidth(s)
	if legacy := ansi.PrintableRuneWidth(xansi.Strip(s)); legacy > width {
		return legacy
	}
	return width
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
