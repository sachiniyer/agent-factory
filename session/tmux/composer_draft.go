package tmux

import (
	"strings"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
)

// ComposerDraftPresent reports whether a captured pane's live composer holds
// unsent text — the draft that blocks an automatic account move (#6776 design
// §5.1). The answer rides on the composer-region machinery the submit remedy
// already trusts: the pane's LAST prompt-glyph row anchors the live input,
// and composerRegion carries whatever sits inside it.
//
// provable reports whether the frame could even be judged: a pane with no
// composer-glyph row at all (mid-redraw, a non-composer screen, a capture of
// raw output) is not evidence of an empty draft, so callers must treat
// unprovable as draft-present. When the composer is found, the verdict is
// honest either way: the glyph row plus interior content is a draft; the bare
// glyph alone is an empty input.
//
// The function reads the capture it is given — it never reaches for the pane.
func ComposerDraftPresent(content string) (present, provable bool) {
	rows := paneGridRows(xansi.Strip(content))
	anchor := lastComposerGlyphRow(rows)
	if anchor < 0 {
		return false, false
	}
	region := normalizeDelivery(strings.Join(composerRegion(rows, anchor), ""))
	// Strip exactly one leading prompt glyph — the payload itself may open
	// with '>' (the same discipline stillChipComposer holds).
	if r, size := utf8.DecodeRuneInString(region); r == '❯' || r == '›' || r == '❭' || r == '>' {
		region = region[size:]
	}
	if region != "" {
		return true, true
	}
	return draftClosedByFrame(rows, anchor), true
}

// draftClosedByFrame disambiguates the shape composerRegion cut short: a
// content row that follows a blank gap below the glyph is EITHER an agent's
// footer/status line (bare idle composer) OR a draft that opened with blank
// lines — textually indistinguishable. The composer's own frame separates
// them: draft text sits INSIDE the border a boxed composer draws (or above
// the rule an unboxed one draws), while a footer lives past the frame's end.
// A border row after the stranded content therefore proves the content was
// interior — a real draft (#6817 review). No border before pane end is a
// footer reading: provably empty stands.
func draftClosedByFrame(rows []string, anchor int) bool {
	sawGap := false
	for j := anchor + 1; j < len(rows); j++ {
		if norm := normalizeDelivery(rows[j]); norm == "" {
			if !blankComposerRow(rows[j]) {
				return false // the frame closed before any stranded content
			}
			sawGap = true
			continue
		} else if !sawGap {
			continue // adjacent interior row — region would have carried it
		}
		// First post-gap content at j: interior draft iff a border row
		// closes the frame below it before the pane ends.
		for k := j + 1; k < len(rows); k++ {
			if nk := normalizeDelivery(rows[k]); nk == "" && !blankComposerRow(rows[k]) {
				return true
			}
		}
		return false
	}
	return false
}
