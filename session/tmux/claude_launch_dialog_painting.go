package tmux

import "strings"

// claudeLegacyTrustPaintedMin is the least of the legacy question af reads as
// that dialog being painted: "Do you", its first row on the narrowest pane
// that shows two words.
const claudeLegacyTrustPaintedMin = len("Doyou")

// claudeLaunchDialogPainting reports the legacy folder-trust dialog while
// Claude Code is still painting it, on a pane too narrow for its question to
// fit a row. claudeLegacyTrustDialogOf reads that question only as one whole
// row, so under 38 columns every frame of the paint read clear.
//
// From some row down to the last content, the pane's text is the start of the
// question, or the whole question, wrapped onto two rows or more, and then
// only rows the dialog paints below it: body text, option rows, its closing
// frame, its footer. A ❯ row there must begin one of the dialog's options; any
// other, or a plain rule, is a composer, and the question is output being
// quoted. A question on one row is left to claudeLegacyTrustDialogOf, which
// reads it whole.
//
// Rows that are the later rows of a composer's draft are the composer's, not
// the dialog's (claudeMCPTrustDraftRow, #7350).
//
// "Do you" alone holds, two common words though they are: on a narrow pane
// they are the whole of the question's first row, and af does not know the
// width Claude Code wraps this dialog at. It is narrower than the pane by a
// margin no capture of this dialog records (#7351 review).
func claudeLaunchDialogPainting(pane claudeLaunchDialogPane) (claudeLaunchDialogMatch, bool) {
	question := claudeLegacyTrustQuestionSquashed
	match := claudeLaunchDialogMatch{kind: claudeLaunchDialogLegacy}
	for first := len(pane.starts) - 1; first >= 0; first-- {
		if !strings.HasPrefix(question, claudeLaunchDialogRowText(pane, first)) ||
			pane.rows[pane.index[first]].selected {
			continue
		}
		text := pane.text[pane.starts[first]:]
		if len(text) < len(question) {
			// The question is still being painted.
			if len(text) >= claudeLegacyTrustPaintedMin && strings.HasPrefix(question, text) &&
				!claudeLaunchDialogAnySelected(pane, first, len(pane.starts)) {
				if claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
					// The later rows of a composer's draft: the composer
					// explains them (#7344, #7350).
					continue
				}
				match.saw = "the start of its question, with nothing painted below it yet"
				if strings.HasPrefix(claudeMCPTrustQuestionSquashed, text) {
					match.kind = claudeLaunchDialogEither
					match.saw = "the first words of a trust question, which the legacy folder-trust dialog and the MCP-server dialog share"
				}
				return match, true
			}
			continue
		}
		if !strings.HasPrefix(text, question) {
			continue
		}
		below, ok := pane.rowStart(pane.starts[first] + len(question))
		if len(text) == len(question) {
			below, ok = len(pane.starts), true
		}
		if !ok || below-first < 2 || claudeLaunchDialogAnySelected(pane, first, below) {
			// The question ends inside a row, or sits on one row.
			continue
		}
		if claudeLaunchDialogComposerBelow(pane, first, below) ||
			claudeMCPTrustDraftRow(pane.lines, pane.rows, pane.index[first], pane.index[len(pane.index)-1]) {
			continue
		}
		match.saw = "its question wrapped onto several rows"
		for i := pane.index[first]; i < len(pane.lines); i++ {
			if claudeLegacyInlinePair(strings.Fields(strings.ToLower(pane.rows[i].label))) {
				// The dialog's option row is the inline pair: name its "Yes",
				// not the stacked form's "Yes, proceed" (#7661 review, R4).
				match.pair = true
			}
		}
		return match, true
	}
	return claudeLaunchDialogMatch{}, false
}
