package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// railHintSeparator is the repo's fragment separator (CLAUDE.md), and it belongs
// to the HINT rather than to the title's trailing space.
//
// That ownership is the whole of #3630. While the leading space lived on the END
// of the title, every shrink of the title ate it and the Automations header
// rendered "Automation…· m manage" — an ellipsis welded to a separator, reading
// as one mangled token rather than a truncated word beside a hint. Owned by the
// hint, it cannot be shortened away.
const railHintSeparator = " · "

// railHelpKey renders a binding's effective glyph from the generated key table,
// so a [keys] rebind surfaces in a rail header exactly as it does in the bottom
// menu and in dispatch (#1026 — one source of truth).
func railHelpKey(name keys.KeyName) string {
	return keys.GlobalKeyBindings[name].Help().Key
}

// railActionHint is one "<key> <verb>" hint fragment.
func railActionHint(name keys.KeyName, desc string) string {
	return railHelpKey(name) + " " + desc
}

// railHeader is a rail section's header text, split at the seam the width ladder
// needs: the noun is decoration and may be shortened or dropped, the counts are
// the only information the header carries and never may be.
type railHeader struct {
	noun   string // "Automations" / "Projects" — or "Automations:" in a compact summary
	counts string // "(2)", or "2 (1 on)" in a compact summary
	// primary is counts reduced to the one number that must survive, and it is a
	// rung of its own before the affordance is touched. A compact summary can
	// carry two numbers ("100 (100 on)" is 12 cells), which at the 22-column rail
	// minimum left nothing for the hint and truncated it — regressing the
	// contract that the affordance is cut last, at a SUPPORTED width rather than
	// below one (#3641 review). Empty means counts is already minimal.
	primary string
}

// text is the header at full width: " Automations (2)".
func (h railHeader) text() string { return " " + h.noun + " " + h.counts }

// countsOnly sheds the noun but keeps the numbers: " (2)".
func (h railHeader) countsOnly() string { return " " + h.counts }

// primaryOnly sheds the secondary number too: " 100" from " 100 (100 on)". It is
// the last rung that still says anything true before clipping.
func (h railHeader) primaryOnly() string {
	if h.primary == "" {
		return ""
	}
	return " " + h.primary
}

// shrunk ellipsizes the NOUN inside w cells while keeping the counts whole, or
// returns "" when there is no room for a noun worth rendering.
func (h railHeader) shrunk(w int) string {
	room := w - layout.Cells(h.countsOnly()) - 1 // 1 for the leading pad
	// Below three cells a "noun" is an ellipsis and a letter or two; drop it and
	// let countsOnly have the width instead.
	if room < 3 {
		return ""
	}
	return " " + fitLine(h.noun, room) + " " + h.counts
}

// railTitleLine renders a rail section header width-aware. hints are already
// separator-prefixed and shed RIGHT-TO-LEFT; hints[0] is the section's key
// affordance and is the last thing cut. Segments go in order of what they cost
// the reader:
//
//	Automations (2) · m manage · e hooks    full
//	Automations (2) · m manage              the trailing hints drop first
//	Automatio… (2) · m manage               then the noun shrinks, counts intact
//	(2) · m manage                          then the noun goes, counts intact
//	100 · m manage                          then the secondary count goes
//
// Three rules hold at every step, and #3630 was the first two failing at once in
// the Automations header while #3642 was the third failing in the Projects one.
//
//  1. The " · " separator is never ellipsized into (see railHintSeparator).
//  2. The counts survive every form. A header that cannot say how many things
//     exist is byte-identical with two of them and with none, which is what both
//     sections rendered at their narrow widths.
//  3. The ladder is MONOTONIC: every rung shows a superset of what the rung below
//     it showed, so widening the rail can never take information away. Projects
//     shed the count at 25 columns and brought it back at 29 — the fallback kept
//     the whole name and dropped the hint, while the rung above it did the
//     reverse.
//
// Rule 3 is what makes sharing this the point rather than a tidy-up: the two
// sections cannot drift into different orders again, and #2580 (a double space
// visible only because the two headers sat in one frame) is the precedent for
// how they drift.
//
// The affordance being cut last is the shipped contract both sections document
// and TestAutomationsTitleWidthAware pins: the key into the section stays
// reachable at the 22-column rail minimum (#1090 width).
func railTitleLine(h railHeader, w int, nameStyle, hintStyle lipgloss.Style, hints ...string) string {
	render := func(title, hint string) string {
		return nameStyle.Render(title) + hintStyle.Render(hint)
	}
	fits := func(title, hint string) bool {
		return layout.Cells(title+hint) <= w
	}

	full := h.text()
	// Shed hints right-to-left, down to (and never past) the sticky one.
	for n := len(hints); n >= 1; n-- {
		if joined := strings.Join(hints[:n], ""); fits(full, joined) {
			return render(full, joined)
		}
	}
	sticky := ""
	if len(hints) > 0 {
		sticky = hints[0]
	}
	if shrunk := h.shrunk(w - layout.Cells(sticky)); shrunk != "" && fits(shrunk, sticky) {
		return render(shrunk, sticky)
	}
	counts := h.countsOnly()
	if fits(counts, sticky) {
		return render(counts, sticky)
	}
	if primary := h.primaryOnly(); primary != "" && fits(primary, sticky) {
		return render(primary, sticky)
	}
	// Narrower than the rail minimum: nothing composes, so clip the whole line
	// rather than pretend one of the pieces still fits.
	return nameStyle.Render(fitLine(counts+sticky, w))
}
