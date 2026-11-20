package tmux

import (
	"slices"
	"strings"
)

// claudeLegacyTrustProceedLabel is the legacy folder-trust dialog's affirmative
// row, and claudeLegacyTrustFooter its footer.
const (
	claudeLegacyTrustProceedLabel = "Yes, proceed"
	claudeLegacyTrustFooter       = "Enter to confirm · Esc to exit"

	// claudeLegacyInlinePairText is the legacy dialog's single-row inline option
	// row, the picker shape claudeLegacyInlinePair matches at full width
	// (session/tmux/claude_trust_legacy.go). The leading "1." ordinal is
	// stripped by trimClaudeTrustOrdinal before the row's label reaches the
	// readers that consume these texts, so this constant carries only the
	// part after it: "Yes  No".
	claudeLegacyInlinePairText = "Yes  No"
	// claudeLegacyInlineOrdinalPairText is the same row with the No option's
	// own ordinal still on it: "Yes  2. No". claudeTrustRowOf's ordinal
	// trimmer drops only the LEADING "N." so the mid-row "2." stays in the
	// label (and survives a squash as "Yes2.No").
	claudeLegacyInlineOrdinalPairText = "Yes  2. No"

	// claudeLegacyInlineYesLabel is the affirmative option the inline pair
	// paints, for the refusal line that names the row the pane shows (#7661
	// review, R4).
	claudeLegacyInlineYesLabel = "Yes"
)

// claudeFolderTrustTailLabels are the option rows the folder-trust dialogs
// paint, as words: the reworded dialog's two, the legacy dialog's "Yes,
// proceed" (its exit row is the reworded dialog's too), and the legacy
// dialog's inline pair ("Yes  No" or "Yes  2. No"). The inline pair was added
// so a pane whose question has scrolled off and whose option row also wraps
// does not read clear the way the stacked form never does — the words of
// the pair are the same at any width, and the ordinal on the No option is
// stripped by the claudeMCPTrustOrdinalWord skip before the words reach this
// reader, so the two inline forms share the one ["Yes", "No"] entry.
var claudeFolderTrustTailLabels = [][]string{
	strings.Fields(claudeTrustAffirmativeLabel),
	strings.Fields(claudeFolderTrustExitLabel),
	strings.Fields(claudeLegacyTrustProceedLabel),
	strings.Fields(claudeLegacyInlinePairText),
}

// claudeFolderTrustTailFooters are the footers those dialogs paint, as words.
var claudeFolderTrustTailFooters = [][]string{
	strings.Fields(claudeFolderTrustFooter),
	strings.Fields(claudeLegacyTrustFooter),
}

// claudeFolderTrustTail reports a pane that shows nothing but the bottom of a
// folder-trust dialog, the reworded one or the legacy one, at any width
// (#7338): every word on the pane, top to bottom, is the dialog's own, and they
// end with its whole footer. Above the footer is at least one word of its
// option block: whole option labels, and at most the tail of one more that the
// top of the pane cut.
//
// It reads words, not rows, because Claude Code's renderer breaks a row only at
// its spaces: a pane too narrow for a row changes which words share a row and
// never the words or their order. claudeFolderTrustHeadless and
// claudeFolderTrustFooterOnly read rows, so under 16 columns, where the footer
// takes three rows and the labels two, neither held; and neither knew the
// legacy dialog's "Yes, proceed" row, so that dialog's option block over its
// footer read clear on a pane of any width.
//
// A working agent's composer cannot leave this: its rules are plain "────"
// rows, which no row here may be, a ❯ row here must begin an option label,
// and any other word on the pane breaks the match. The verdict it feeds is
// claudeFolderTrustUnreadable: hold, send nothing, say why once.
func claudeFolderTrustTail(lines []string, rows []claudeTrustRow) bool {
	type word struct {
		text string
		row  int
	}
	var words []word
	cursors := map[int]bool{} // index into words where a ❯ row begins
	for i, row := range rows {
		if claudeComposerRule(lines[i]) {
			return false
		}
		if row.blank {
			continue
		}
		if row.selected {
			cursors[len(words)] = true
		}
		for _, text := range strings.Fields(row.label) {
			if !claudeMCPTrustOrdinalWord(text) {
				words = append(words, word{text, i})
			}
		}
	}
	texts := make([]string, len(words))
	for i, w := range words {
		texts[i] = w.text
	}

	end := -1
	for _, footer := range claudeFolderTrustTailFooters {
		if n := len(texts) - len(footer); n >= 0 && slices.Equal(texts[n:], footer) {
			end = n
		}
	}
	if end <= 0 {
		// No footer, or the footer and nothing above it: that pane is
		// claudeFolderTrustFooterOnly's to read.
		return false
	}
	// Whole labels from the footer up; what is left at the top of the pane
	// must be the tail of one label.
	starts := map[int]bool{}
	at := end
	for at > 0 {
		matched := false
		for _, label := range claudeFolderTrustTailLabels {
			if n := at - len(label); n >= 0 && slices.Equal(texts[n:at], label) {
				at, matched = n, true
				starts[n] = true
				break
			}
		}
		if matched {
			continue
		}
		cut := false
		for _, label := range claudeFolderTrustTailLabels {
			if at < len(label) && slices.Equal(texts[:at], label[len(label)-at:]) {
				cut = true
			}
		}
		if !cut {
			return false
		}
		break
	}
	// The cursor sits on an option row: a ❯ row begins a label.
	for at := range cursors {
		if !starts[at] {
			return false
		}
	}
	return true
}
