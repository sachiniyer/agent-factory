package layout

import (
	"strings"
)

// ClampToRect pads and truncates s to exactly r.W×r.H terminal cells: the
// result is exactly r.H lines, each exactly r.W columns in the measure the
// rectangle contract is enforced in. This is the shared enforcement helper for
// the Pane contract "View() is exactly Rect-sized" (RFC §2.6), replacing the
// per-pane ad-hoc clamps.
//
// Truncation keeps the first r.H lines and the leading r.W columns of each
// line — a View renders top-down, so overflow is always trailing chrome;
// callers with keep-newest semantics (scrollback) trim before clamping.
// Width handling is ANSI- and wide-rune-aware: escape sequences are
// preserved and measured at zero width, and a truncated styled line gets a
// reset appended so leaked styles cannot bleed into the padding or the
// neighboring region. Only r's size is used; its position is ignored.
//
// # What "exactly r.W" promises, and what it does not
//
// For every row af's own chrome draws and every ASCII/CJK title — anything the
// width functions agree on — the row is exactly r.W cells on screen, as before.
//
// For a row carrying content they disagree about, the promise is one-sided:
// the row NEVER exceeds r.W, and may fall short of it by up to the amount the
// measures differ. That is the #3614 decision — an overflow wraps and makes
// every height budget above it a lie (#3430), a gap is cosmetic — and it is why
// such a row is fitted and padded by CellsUpperBound instead. See contract.go.
// A right-edge affix must be laid out with RowWithRightAffix so the shortening
// lands on the elastic middle rather than on the affix.
//
// An empty rect clamps everything to "".
func ClampToRect(s string, r Rect) string {
	if r.Empty() {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > r.H {
		lines = lines[:r.H]
	}
	out := make([]string, r.H)
	for i := range out {
		var line string
		if i < len(lines) {
			line = lines[i]
		}
		// Fit AND pad on the SAME measure, which is the contract measure rather
		// than Cells. Two ways that has bitten: x/ansi's truncator can return a
		// row that still measures over, so the pad is skipped and the row goes
		// out narrow (#3585 review); and padding to a measure that under-counts
		// a grapheme pushes the row past the rectangle it just claimed to fit
		// (#3614). One measure on both sides is what closes them.
		out[i] = padToContract(clipToContract(line, r.W), r.W)
	}
	return strings.Join(out, "\n")
}

// ClampToRectMarkingCut is ClampToRect with the repo's truncation marker: any
// row that had to be shortened gets "…" in its last cell instead of a silent
// hard cut (#4175). Use it where the clipped content is user data — captured
// pane rows from a wider source, like a preview-only session's 80-column pane
// rendered into a narrower preview box — rather than af's own chrome, which
// elides its own text before the clamp sees it.
//
// The marker is a re-cut, not an affix: the content keeps r.W-1 cells and the
// last cell carries the "…", so the row still measures exactly r.W and a style
// the content cut left open is closed before the marker, never around it.
func ClampToRectMarkingCut(s string, r Rect) string {
	if r.Empty() {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > r.H {
		lines = lines[:r.H]
	}
	out := make([]string, r.H)
	for i := range out {
		var line string
		if i < len(lines) {
			line = lines[i]
		}
		out[i] = MarkCutRow(line, r.W)
	}
	return strings.Join(out, "\n")
}

// MarkCutRow enforces the width contract on ONE row the way
// ClampToRectMarkingCut does: a row wider than width is re-cut so its last
// cell carries "…" (#4175); a row that fits is padded out to width unchanged.
//
// The single-row form is for callers marking raw capture rows BEFORE styling:
// a lipgloss Render pads every row of a block to its widest line, so a marker
// applied afterwards would read that padding as content and stamp "…" on rows
// that fit. Mark first, then render.
func MarkCutRow(s string, width int) string {
	return padToContract(clipMarkingCut(s, width), width)
}

// clipMarkingCut is clipToContract that spends the row's last cell on "…" when
// the row had to be shortened. Rows that already fit are returned unchanged, so
// a row exactly at width is never mistaken for a cut one.
func clipMarkingCut(s string, width int) string {
	if contractCells(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	// clipToContract closes an open style with a reset, so the marker lands
	// after it and renders as chrome rather than in the cut row's last style.
	return clipToContract(s, width-1) + "…"
}
