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
// Placeholder ghost text is empty input, never a draft (#6817 review): a
// composer's "Ask Codex to do anything" or `Try "…"` hint survives the ANSI
// strip as plain text but is still nobody's typing. A glyph af does not
// recognize — Ultra's bare "»" — stays unprovable rather than claiming a
// verdict.
//
// The function reads the capture it is given — it never reaches for the pane.
func ComposerDraftPresent(content string) (present, provable bool) {
	rows := paneGridRows(xansi.Strip(content))
	anchor := lastComposerGlyphRow(rows)
	if anchor < 0 {
		if ultraPlaceholderRowPresent(rows) {
			return false, true
		}
		return false, false
	}
	// The glyph row's own text decides before the region is joined: an empty
	// composer's ghost placeholder proves EMPTY even with a footer row the
	// region would have folded into the payload — but only by EXACT known
	// text (#6817 round-2 review). Ghost text is only ever drawn into an
	// empty input, so an exact placeholder sighting is provably empty; a
	// placeholder-SHAPED row af cannot match exactly — a rotated hint or a
	// draft that merely opens the same way — stays unprovable, never empty.
	glyphText := afterComposerGlyph(normalizeDelivery(rows[anchor]))
	switch {
	case composerPlaceholderText(glyphText):
		return false, true
	case composerPlaceholderShaped(glyphText):
		return false, false
	}
	regionRows := composerRegion(rows, anchor)
	region := afterComposerGlyph(normalizeDelivery(strings.Join(regionRows, "")))
	if composerPlaceholderShaped(region) {
		return false, false
	}
	if region != "" {
		return true, true
	}
	// normalizeDelivery erases box-drawing and block runes (0x2500–0x259F)
	// to drop composer borders — but a draft DRAWN in those runes erases
	// with them, so `❯ ███` would read as an empty input (#6817 review).
	// Any residual left after peeling the frame is unprovable — border
	// fragment or typed blocks, af cannot tell — never empty.
	if composerBoxResidual(regionRows) {
		return false, false
	}
	// An empty region under a bare-glyph anchor is only as good as the
	// anchor choice: a `>`-only continuation row inside a wrapped draft is
	// indistinguishable from a fresh bare prompt, and the draft's earlier
	// lines sit ABOVE the anchor the analyzer picked (#6817 review). A
	// glyph-carrying content row in the same frame makes the reading
	// ambiguous — unprovable, never empty.
	if composerAnchorAmbiguous(rows, anchor) {
		return false, false
	}
	return draftClosedByFrame(rows, anchor), true
}

// afterComposerGlyph strips exactly one leading prompt glyph from normalized
// composer text — the payload itself may open with '>' (the same discipline
// stillChipComposer holds).
func afterComposerGlyph(s string) string {
	if r, size := utf8.DecodeRuneInString(s); r == '❯' || r == '›' || r == '❭' || r == '>' {
		return s[size:]
	}
	return s
}

// composerPlaceholders is the set of NORMALIZED placeholder texts af has seen
// an agent draw as ghost hint in an empty composer — exact matches only
// (#6817 round-2 review). Codex's hint is a fixed string. Claude rotates its
// `Try "…"` suggestions, so only observed texts are listed; a hint af cannot
// prove is placeholder-shaped, and shaped is unprovable.
var composerPlaceholders = map[string]bool{
	"AskCodextodoanything": true, // codex — both the old › and Ultra » glyphs
	`Try"howdoI…"`:         true, // claude hint
}

// composerPlaceholderText reports whether NORMALIZED composer interior text
// is an EXACT known placeholder — nothing more, nothing after it.
func composerPlaceholderText(region string) bool {
	return composerPlaceholders[region]
}

// composerPlaceholderShaped reports whether NORMALIZED composer interior text
// OPENS like a placeholder hint without being an exact match — a rotated
// `Try "…"` suggestion or a leading "Ask Codex" — so the frame cannot be
// called empty and cannot be called a draft either: unprovable.
func composerPlaceholderShaped(region string) bool {
	return strings.HasPrefix(region, `Try"`) || strings.HasPrefix(region, "AskCodex")
}

// ultraPlaceholderRowPresent handles the composer whose glyph is not a prompt
// anchor — Ultra's » (Codex draws it only under the Ultra tier, #5383). Its
// ghost-placeholder row is still a definitive empty-composer sighting: the
// hint text is never submitted, so an exact match cannot be a transcript
// echo. A bare » or a » carrying anything else stays unprovable — submitted
// prompts echo with the same glyph, so non-placeholder text proves nothing.
func ultraPlaceholderRowPresent(rows []string) bool {
	for _, row := range rows {
		norm := normalizeDelivery(row)
		rest, ok := strings.CutPrefix(norm, "»")
		if !ok {
			continue
		}
		if composerPlaceholderText(rest) {
			return true
		}
	}
	return false
}

// composerBoxResidual reports whether a composer region still holds visible
// content once the frame is peeled away — a whole row of box runes is a
// border and skipped, then one border column is trimmed from each surviving
// row's edges, and the anchor row sheds its prompt glyph. Whatever remains
// is text normalizeDelivery erased without being a frame: a border
// fragment, or a draft typed in box-drawing/block runes — af cannot tell,
// so its existence is unprovable, never empty (#6817 review).
func composerBoxResidual(regionRows []string) bool {
	for i, row := range regionRows {
		s := strings.TrimSpace(row)
		if s == "" || composerAllBoxRunes(s) {
			continue
		}
		s = strings.TrimSpace(trimOneBoxRune(s, true))
		s = strings.TrimSpace(trimOneBoxRune(s, false))
		if i == 0 {
			s = strings.TrimSpace(afterComposerGlyph(s))
		}
		if s != "" {
			return true
		}
	}
	return false
}

// composerAllBoxRunes reports whether every rune is box-drawing or a block
// element — a whole frame row.
func composerAllBoxRunes(s string) bool {
	for _, r := range s {
		if r < 0x2500 || r > 0x259F {
			return false
		}
	}
	return true
}

// trimOneBoxRune drops a single box-drawing rune at the front (or back) of
// an already space-trimmed string — the one border column at that edge.
func trimOneBoxRune(s string, front bool) string {
	if s == "" {
		return s
	}
	if front {
		if r, size := utf8.DecodeRuneInString(s); r >= 0x2500 && r <= 0x259F {
			return s[size:]
		}
		return s
	}
	if r, size := utf8.DecodeLastRuneInString(s); r >= 0x2500 && r <= 0x259F {
		return s[:len(s)-size]
	}
	return s
}

// composerAnchorAmbiguous reports whether a GLYPH-carrying row immediately
// above the anchor holds content — the shape a wrapped draft leaves when its
// last line is bare (a `>` quote line reads as a fresh bare prompt and wins
// the anchor). Only contiguous glyph rows count: a submitted prompt echoes
// with the same glyph, but output sits between the echo and the live prompt,
// so the first non-glyph row ends the scan (#6817 review).
func composerAnchorAmbiguous(rows []string, anchor int) bool {
	for j := anchor - 1; j >= 0; j-- {
		norm := normalizeDelivery(rows[j])
		if norm == "" {
			return false
		}
		r, _ := utf8.DecodeRuneInString(norm)
		if r != '❯' && r != '›' && r != '❭' && r != '>' {
			return false
		}
		if afterComposerGlyph(norm) != "" {
			return true
		}
	}
	return false
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
