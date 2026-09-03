package layout

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// The fixed-rectangle contract, and the one place its measure differs from Cells.
//
// ClampToRect promises a rectangle of exactly r.W cells per row, and it keeps
// that promise by padding a short row up to r.W in whatever measure it trusts. So
// when the measure UNDER-counts a grapheme, the pad is computed from a width the
// row does not have, and the padded row is emitted WIDER than the rectangle
// promised. One chained ZWJ family is enough, and the row never looks too wide on
// the way in, so #3578's clip never sees it (#3614):
//
//	"ab" + <ZWJ family> + "cd" into a 10-cell rectangle
//	                       reports  pads  row claims  tmux 3.4 draws
//	lipgloss.Width               6     4          10            12
//	xansi.StringWidth (Cells)    6     4          10            12
//
// The decision on #3614 is that the contract is "NEVER OVERFLOW", and under-
// filling is the accepted cost. An overflow wraps, and a wrapped row makes every
// height budget above it a lie (#3430); a gap is cosmetic. So a row whose width
// cannot be trusted is fitted AND padded by an explicit upper bound: true width ≤
// CellsUpperBound, so true + pad ≤ W and the row cannot leave the rectangle. The
// price is up to (bound − true) blank cells at the end of that one row.
//
// # Known-unreliable is DETECTED, not modelled
//
// contentMeasuresDisagree asks a question about the STRING — "does this contain
// the class of code point the width functions disagree about?" — not about how
// tmux would cluster it. That distinction is the whole reason this is not
// direction (1) on the issue, which #3614 rejects: modelling tmux's clustering
// (it collapses a family to 4, a couple to 2, a profession to 2 — all measured)
// is the predicate-over-a-grammar trap #3578 hit six times, and it would be
// tmux-version-specific besides. A code-point test is neither. It is allowed to
// be conservative: a false positive costs a few blank cells on one row.
//
// Rows without those code points — every row af's own chrome draws, and every
// ASCII/CJK title — take Cells exactly as before. That is pinned.

// contentMeasuresDisagree reports whether s contains a code point from the class
// the available width functions are known to disagree about.
//
// Every entry below is a carrier #3610's corpus MEASURED a disagreement on, with
// the numbers beside it. That is the admission test, and it is not "this code
// point is in an interesting Unicode category": a new range belongs here when
// someone has measured the width functions splitting on it, not when a table
// suggests they might. Completing the list from General_Category would pull in
// ordinary script text and cost real title cells for nothing — see what is
// deliberately excluded, below.
//
// The three functions (x/ansi, runewidth, PrintableRuneWidth) agree on plain
// text, CJK, Hangul and fullwidth latin, and split on the multi-code-point EMOJI
// cluster:
//
//	U+200D          zero-width joiner — chained emoji (x/ansi 2, PRW 8, tmux 4)
//	U+FE0F          variation selector 16 (x/ansi 2, PRW 1, tmux 2)
//	U+1F3FB–U+1F3FF emoji skin-tone modifiers (x/ansi 2, PRW 4, tmux 2)
//	U+E0020–U+E007F emoji TAG sequences — the subdivision flags
//
// It is a fact about the bytes, so it stays true whatever tmux version is on the
// other end.
//
// # The tag range is an addition to #3614's literal list, and why
//
// The triage named the first three. The fourth was found by sweeping the same
// question over a wider corpus: the England/Scotland/Wales flags are a base
// U+1F3F4 followed by tag characters, carry NONE of the first three code points,
// and measure 2 to x/ansi against 8 to PrintableRuneWidth — the identical shape
// and the identical size of disagreement as the chained family this issue is
// about. Leaving them out would be a silent overflow of exactly the kind being
// fixed, on content a user can put in a session title. Tag characters appear in
// nothing but emoji, so admitting them costs nothing anywhere else.
//
// # What is deliberately NOT here
//
// The same sweep found ordinary SCRIPT clusters where the measures also disagree
// — Devanagari "\u0915\u094d\u0937" (x/ansi 1, PRW 3), Thai "\u0e01\u0e33"
// (1 against 2), a Hangul jamo cluster (2 against 4). They are excluded on
// purpose, and the exclusion is the same judgement #3610 made when it withdrew
// the blanket overestimate: there PrintableRuneWidth OVER-counts ordinary text
// badly, so bounding by it would shorten real titles in those scripts on every
// row that carries them, to buy a rectangle guarantee against a disagreement
// nobody has measured tmux on. Predicted harm on emoji is worth the blank cells;
// measured harm on a user's own language is not.
//
// That is a gap, stated rather than hidden: if tmux is measured advancing more
// than x/ansi reports for one of those clusters, the fix is to add the range here
// with the measurement beside it, exactly as the emoji rows above carry theirs.
func contentMeasuresDisagree(s string) bool {
	for _, r := range s {
		switch {
		case r == '\u200d', r == '\ufe0f': // ZWJ, variation selector 16
			return true
		case r >= '\U0001F3FB' && r <= '\U0001F3FF': // emoji skin-tone modifiers
			return true
		case r >= '\U000E0020' && r <= '\U000E007F': // emoji tag sequences
			return true
		}
	}
	return false
}

// contractCells is the width the fixed-rectangle contract budgets s at: Cells for
// every ordinary row, and CellsUpperBound for one carrying content the measures
// disagree about. It is deliberately NOT exported and NOT a rival to Cells — Cells
// remains the ONE width answer for placing, clipping and hit-testing (#3585,
// #3610). This is only what "fits the rectangle" means to the code that pads a
// row out to the rectangle's edge.
func contractCells(s string) int {
	if contentMeasuresDisagree(s) {
		return CellsUpperBound(s)
	}
	return Cells(s)
}

// truncateToContract shortens s until contractCells fits width, without ever
// cutting through a control sequence or splitting a grapheme cluster.
//
// Same loop as TruncateToCells, against the contract measure — and here the
// truncator and the measure genuinely do disagree, which is why asking x/ansi
// once for `width` is not enough: for unreliable content it counts fewer cells
// than the contract budgets, so its answer for `width` can still be over budget.
func truncateToContract(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if contractCells(s) <= width {
		return s
	}
	for w := width; w >= 0; w-- {
		out := xansi.Truncate(s, w, "")
		if contractCells(out) <= width {
			return out
		}
	}
	return ""
}

// padToContract pads s with blanks up to width in the contract measure. A string
// already at or over width is returned unchanged.
func padToContract(s string, width int) string {
	if short := width - contractCells(s); short > 0 {
		return s + strings.Repeat(" ", short)
	}
	return s
}

// RowWithRightAffix lays out one row of exactly width cells with affix pinned to
// the RIGHT edge and flex filling everything left of it.
//
// This is the structural half of #3614, and without it the upper bound above is
// not safe to use. A fixed affix must not be part of the flowed text that
// truncation acts on: the sidebar's status row is
//
//	<title, elastic> <space> <● status glyph>
//
// and the glyph is the last thing on the line, so ANY right-to-left truncation of
// the assembled row takes the glyph first. That is not hypothetical — it is the
// measured loss that made #3610 withdraw the overestimate as the general measure:
// a session titled "fam<family>zzz" measured 36 against a real 30, the clamp cut
// the tail, and every session whose title carries clustered emoji silently lost
// its status marker. Budgeting the affix from the edge instead means an
// over-estimated title costs a SHORTER TITLE, which is what elides everywhere
// else in this tree, and the dot stays.
//
// The same reasoning is already written out on RenderTab's " *" active marker
// (#1983): content elides, indicators do not. This is that rule with the
// rectangle's own measure behind it.
//
// The result measures exactly width by contractCells, so it neither overflows the
// rectangle nor arrives at ClampToRect looking as though it does. Truncation here
// is silent — no "…" — because it is the last-resort enforcement, not the
// presentation: callers elide their own text first (tree.Render marks its cut
// with "…" before handing the row over) and this only takes back the cells an
// unreliable measure cannot account for.
func RowWithRightAffix(flex, affix string, width int) string {
	if width <= 0 {
		return ""
	}
	affixWidth := contractCells(affix)
	if affixWidth >= width {
		// Narrower than its own affix: there is no elastic middle left to spend,
		// so the affix takes the row and degrades with it.
		return padToContract(clipToContract(affix, width), width)
	}
	return padToContract(clipToContract(flex, width-affixWidth), width-affixWidth) + affix
}

// RowKeepingTail is RowWithRightAffix for an indicator that belongs NEXT TO its
// content rather than at the rectangle's edge: tail follows flex immediately and
// the blanks go after both.
//
// Same reservation, same reason — the width for tail is taken out of flex's
// budget in the contract measure, so the row arrives at the clamp already inside
// the rectangle and the clamp has no tail to take. The difference is only where
// the slack sits, and that is a design fact about the indicator, not about the
// measure: the sidebar's ● status glyph IS the right-hand column of its row, and
// a tab row's " *" active marker is a tmux-style cue appended to the tab's name
// (#1983). Moving either one to the other's position would be a redesign.
func RowKeepingTail(flex, tail string, width int) string {
	if width <= 0 {
		return ""
	}
	tailWidth := contractCells(tail)
	if tailWidth >= width {
		return padToContract(clipToContract(tail, width), width)
	}
	return padToContract(clipToContract(flex, width-tailWidth)+tail, width)
}

// clipToContract truncates to the contract measure and closes any style the cut
// left open, so a truncated styled row cannot bleed into the padding after it or
// into the neighbouring region.
func clipToContract(s string, width int) string {
	if contractCells(s) <= width {
		return s
	}
	out := truncateToContract(s, width)
	if strings.Contains(out, "\x1b") {
		out += "\x1b[0m"
	}
	return out
}
