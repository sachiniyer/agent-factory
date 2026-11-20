package tmux

import (
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// The remnant reader (#7618 item 2). A settled folder-trust dialog, the
// reworded one or the legacy one, could still read clear in three shapes every
// reader before this one missed, and a clear pane is one the create types its
// prompt into:
//
//   - On a pane under about 17 columns Claude Code no longer re-wraps the
//     dialog's option rows. It overprints and cuts them: "1.Yes, I" over
//     "❯  trust th" over "folder", or "1Y" over "❯ ," at four columns. The
//     question, the body and the footer still re-wrap cleanly around them. So
//     no reader found an option label above the footer, and Claude Code
//     2.1.328 on a pane of six to nine rows, which paints the cursor's
//     "No, exit" row alone, read the same way (claudeLaunchDialogUnderGuide).
//   - After one Ctrl-C the dialog's footer row reads "Press Ctrl-C again to
//     exit" for a moment. The reworded grammar holds that row
//     (claudeFolderTrustBroken); the legacy grammar read it as a row the
//     dialog never paints, and the pane as no dialog
//     (claudeLaunchDialogPendingExit).
//   - A pane too small for anything but the end of the dialog's footer shows
//     "· Esc" over "to" over "cancel" and nothing else
//     (claudeLaunchDialogFooterTail).
//
// It is asked last of all, after every clause that answers or holds, so it
// only turns a clear pane into a hold. It sends no key.

// claudeLaunchDialogRemnantKind names which remnant the reader saw.
type claudeLaunchDialogRemnantKind int

const (
	claudeLaunchDialogRemnantNone claudeLaunchDialogRemnantKind = iota
	// claudeLaunchDialogRemnantUnderGuide: the dialog's guide row, what a
	// narrow or short pane leaves of its option rows, and its whole footer.
	claudeLaunchDialogRemnantUnderGuide
	// claudeLaunchDialogRemnantPendingExit: the dialog with its footer row
	// reading "Press Ctrl-C again to exit".
	claudeLaunchDialogRemnantPendingExit
	// claudeLaunchDialogRemnantFooterTail: nothing but the end of the
	// dialog's footer.
	claudeLaunchDialogRemnantFooterTail
)

// claudeLaunchDialogGuideRows are the rows the folder-trust dialogs paint
// straight above their option block, squashed: the reworded wording's
// "Security guide" and the legacy wording's "Learn more". Builds through
// 2.0.x print the link's URL after it in brackets
// (testdata/claude_2_0_25_legacy_folder_trust_dialog.ansi); later ones link
// the words.
var claudeLaunchDialogGuideRows = []string{"Securityguide", "Learnmore"}

// claudeLaunchDialogGuideRowsWhole are those rows as a build paints them
// whole, squashed: the words alone, and the words with each URL a build has
// printed after them (2.0.25's and 2.0.50's; 2.0.50 pads its URL with
// zero-width spaces, which the reader drops).
var claudeLaunchDialogGuideRowsWhole = func() []string {
	var rows []string
	for _, row := range claudeLaunchDialogGuideRows {
		rows = append(rows, row,
			row+"(https://docs.claude.com/s/claude-code-security)",
			row+"(https://code.claude.com/docs/en/security)")
	}
	return rows
}()

// claudeLaunchDialogOptionBlocks are the option blocks those dialogs paint,
// squashed, ordinals included: affirmative first (every build through
// 2.1.2xx, and the legacy wording), and "No, exit" first (2.1.257 and 2.1.258,
// and 2.1.323 on, which dropped the ordinals).
var claudeLaunchDialogOptionBlocks = []string{
	claudeSquash("1. " + claudeTrustAffirmativeLabel + " 2. " + claudeFolderTrustExitLabel),
	claudeSquash("1. " + claudeFolderTrustExitLabel + " 2. " + claudeTrustAffirmativeLabel),
	claudeSquash("1. " + claudeLegacyTrustProceedLabel + " 2. " + claudeFolderTrustExitLabel),
}

// claudeLaunchDialogRemnantOf reports a folder-trust dialog the pane shows a
// remnant of, in any of the shapes above.
func claudeLaunchDialogRemnantOf(screen visibleText) claudeLaunchDialogRemnantKind {
	if claudeLaunchDialogUnderGuide(screen) {
		return claudeLaunchDialogRemnantUnderGuide
	}
	if claudeLaunchDialogPendingExit(screen) {
		return claudeLaunchDialogRemnantPendingExit
	}
	if claudeLaunchDialogFooterTail(screen) {
		return claudeLaunchDialogRemnantFooterTail
	}
	return claudeLaunchDialogRemnantNone
}

// claudeLaunchDialogUnderGuide reports a pane whose content ends with the
// bottom of a folder-trust dialog as a pane too narrow or too short for its
// option rows leaves it. Read with every space and break removed
// (claudeSquash), from some row down to the last content the pane's text is
// exactly:
//
//	guide row   "Security guide" or "Learn more", beginning a row, then the
//	            bracketed URL the builds that print one put after it
//	options     one character or more, each a character of one of the
//	            dialog's option blocks and no more of each than that block
//	            holds (claudeLaunchDialogOptionBlocks). A narrow pane drops
//	            characters from those rows, and at four columns paints two
//	            columns of a row side by side ("2N" over ".o"), and never
//	            adds any; a short pane paints only the cursor's row
//	footer      a whole folder-trust footer, beginning a row, the last
//	            content, and no ❯ row
//
// The guide row may be missing, or cut by the top of the pane, only when
// nothing is above it: the 2.0.25 dialog at five columns paints a character a
// row, and 60 rows do not reach up to its guide row. Then the pane's whole
// text is the end of a guide row as a build paints it whole
// (claudeLaunchDialogGuideRowsWhole), if any of it shows, the option
// characters, and the footer.
//
// Both ends are the dialog's own rows, whole, and nothing between them is
// anything else, so a transcript that only mentions "Security guide" does not
// read this way. A working agent's composer cannot leave it either: the
// composer is the last thing in its pane, so a dialog quoted above one has the
// composer's rows after its footer, and a plain rule from the guide row down,
// which only a composer draws, keeps the pane clear.
func claudeLaunchDialogUnderGuide(screen visibleText) bool {
	pane := claudeLaunchDialogPaneOf(screen)
	end := -1
	for _, footer := range claudeLaunchDialogFooters {
		if strings.HasSuffix(pane.text, footer) {
			end = len(pane.text) - len(footer)
		}
	}
	if end <= 0 {
		return false
	}
	footerRow, ok := pane.rowStart(end)
	if !ok || claudeLaunchDialogAnySelected(pane, footerRow, len(pane.starts)) || claudeLaunchDialogBoxClosesLast(screen) {
		return false
	}
	for guide := footerRow - 1; guide >= 0; guide-- {
		if pane.rows[pane.index[guide]].selected {
			continue
		}
		between := pane.text[pane.starts[guide]:end]
		for _, row := range claudeLaunchDialogGuideRows {
			options, ok := strings.CutPrefix(between, row)
			if !ok {
				continue
			}
			if rest, bracketed := strings.CutPrefix(options, "("); bracketed {
				_, after, closed := strings.Cut(rest, ")")
				if !closed {
					continue
				}
				options = after
			}
			if claudeLaunchDialogOptionsRemnant(options) && !claudeLaunchDialogRuleFrom(pane, pane.index[guide]) {
				return true
			}
		}
	}
	// No whole guide row: the option rows are the first content the pane
	// shows, or the end of the guide row the top of the pane cut is.
	if claudeLaunchDialogRuleFrom(pane, 0) {
		return false
	}
	head := strings.ReplaceAll(pane.text[:end], "\u200b", "")
	if claudeLaunchDialogOptionsRemnant(head) {
		return true
	}
	for _, row := range claudeLaunchDialogGuideRowsWhole {
		for cut := 1; cut < len(row); cut++ {
			if options, ok := strings.CutPrefix(head, row[cut:]); ok && claudeLaunchDialogOptionsRemnant(options) {
				return true
			}
		}
	}
	return false
}

// claudeLaunchDialogOptionsRemnant reports whether text is what a pane leaves
// of one of the dialogs' option blocks: not empty, and every character in it
// is one of that block's, no more of each than the block holds.
func claudeLaunchDialogOptionsRemnant(text string) bool {
	if text == "" {
		return false
	}
	for _, block := range claudeLaunchDialogOptionBlocks {
		if claudeCharactersOf(text, block) {
			return true
		}
	}
	return false
}

// claudeCharactersOf reports whether whole holds every character of part, as
// many times as part does.
func claudeCharactersOf(part, whole string) bool {
	left := map[rune]int{}
	for _, r := range whole {
		left[r]++
	}
	for _, r := range part {
		if left[r] == 0 {
			return false
		}
		left[r]--
	}
	return true
}

// claudeLaunchDialogFooterTailMinWords is the least of a footer's end af reads
// as that footer: its last three words, "Esc to cancel" or "Esc to exit".
// Less ("to cancel", "cancel", "el") is how any text may end.
const claudeLaunchDialogFooterTailMinWords = 3

// claudeLaunchDialogFooterEnd reports whether text, squashed, is the end of a
// folder-trust footer: the footer from some character on, a narrow pane
// breaking it inside a word as readily as between two, and at least its last
// claudeLaunchDialogFooterTailMinWords words.
func claudeLaunchDialogFooterEnd(text string) bool {
	for _, footer := range claudeTrustFooters {
		words := strings.Fields(footer)
		least := strings.Join(words[len(words)-claudeLaunchDialogFooterTailMinWords:], "")
		if len(text) >= len(least) && strings.HasSuffix(claudeSquash(footer), text) {
			return true
		}
	}
	return false
}

// claudeLaunchDialogFooterTail reports a pane that shows nothing but the end
// of a folder-trust footer: its text, top to bottom, is the footer's own, from
// some character of it to its last, on any number of rows. Claude Code
// keeps the bottom of a dialog on a pane too small for it, so a pane of two to
// four rows and a few columns shows "· Esc" over "to" over "cancel".
//
// The whole footer alone reads this way too, on the rows a narrow pane wraps
// it onto. No ❯ row and no plain rule may be on the pane: a working agent's
// composer paints both, and a pane that shows a composer shows more than this.
// Nor may a box's bottom edge be the last thing painted
// (claudeLaunchDialogBoxClosesLast).
func claudeLaunchDialogFooterTail(screen visibleText) bool {
	pane := claudeLaunchDialogPaneOf(screen)
	if !claudeLaunchDialogFooterEnd(pane.text) || claudeLaunchDialogBoxClosesLast(screen) ||
		claudeLaunchDialogAnySelected(pane, 0, len(pane.starts)) || claudeLaunchDialogRuleFrom(pane, 0) {
		return false
	}
	return true
}

// claudeLaunchDialogBoxClosesLast reports whether the last painted line of the
// pane is a box's bottom edge. No build paints its footer inside a box: 2.0.25,
// the one that boxes the dialog, paints the footer under the box
// (testdata/claude_2_0_25_legacy_folder_trust_dialog.ansi). A footer with a
// box closing under it is text in a boxed composer's draft whose prompt row
// has scrolled off, and a hold for a quoted draft is #7648's to decide.
func claudeLaunchDialogBoxClosesLast(screen visibleText) bool {
	lines := screen.lines()
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return claudeTrustFrameClosingRule(lines[i])
		}
	}
	return false
}

// claudeLaunchDialogPendingExitMaxKey bounds the key name in "Press <key>
// again to exit", squashed: "Ctrl-C" and "Ctrl-D" are the two Claude Code
// paints.
const claudeLaunchDialogPendingExitMaxKey = 12

// claudeLaunchDialogPendingExit reports a folder-trust dialog whose footer row
// reads "Press <key> again to exit": one exit key was pressed in the pane, and
// Claude Code shows the notice where the footer was until the press times out
// (#7275 review).
//
// The notice is the pane's last content, on a row of its own or wrapped onto
// several. The pane with those rows read as a whole footer, each of the two
// the dialogs paint in turn, must then be one a folder-trust reader that reads
// a dialog to the bottom of the pane answers or holds: the legacy grammar,
// the re-wrap matcher, the headless readers, or claudeLaunchDialogUnderGuide.
// So the notice proves nothing alone, and every guard those readers keep
// against a quoted dialog applies here unchanged.
//
// A working agent's composer paints the same notice in its status row. That
// row is straight under the composer's closing line, a plain rule or its
// box's bottom edge, and a dialog paints neither straight above its footer
// except the bottom edge of its own box, which holds no prompt row. So a
// notice straight under a composer's closing line is the composer's, and the
// pane stays clear.
func claudeLaunchDialogPendingExit(screen visibleText) bool {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	last := -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if !rows[i].blank {
			last = i
		}
	}
	if last < 0 {
		return false
	}
	for first := last; first >= 0 && !rows[first].blank && !rows[first].selected; first-- {
		var notice strings.Builder
		for i := first; i <= last; i++ {
			notice.WriteString(claudeSquash(rows[i].label))
		}
		if !claudeLaunchDialogPendingExitNotice(notice.String()) {
			continue
		}
		if claudeLaunchDialogNoticeUnderComposer(lines, rows, first) || !claudeLaunchDialogPaintedAbove(rows, first) {
			return false
		}
		for _, footer := range claudeTrustFooters {
			if claudeFolderTrustReadToBottom(claudeLaunchDialogFooterInPlace(lines, first, last, footer)) {
				return true
			}
		}
		return false
	}
	return false
}

// claudeLaunchDialogPaintedAbove reports whether any row above line i is
// painted. The notice alone on a pane is no part of a dialog af can name.
func claudeLaunchDialogPaintedAbove(rows []claudeTrustRow, i int) bool {
	for _, row := range rows[:i] {
		if !row.blank {
			return true
		}
	}
	return false
}

// claudeLaunchDialogPendingExitNotice reports whether text, squashed, is
// "Press <key> again to exit".
func claudeLaunchDialogPendingExitNotice(text string) bool {
	key, ok := strings.CutPrefix(text, "Press")
	if !ok {
		return false
	}
	key, ok = strings.CutSuffix(key, "againtoexit")
	return ok && key != "" && len(key) <= claudeLaunchDialogPendingExitMaxKey
}

// claudeLaunchDialogNoticeUnderComposer reports whether the nearest painted
// line above line notice is a composer's closing line: a plain rule, or the
// bottom edge of a box that holds a composer's prompt row.
func claudeLaunchDialogNoticeUnderComposer(lines []string, rows []claudeTrustRow, notice int) bool {
	above := notice - 1
	for above >= 0 && strings.TrimSpace(lines[above]) == "" {
		above--
	}
	if above < 0 {
		return false
	}
	if claudeComposerRule(lines[above]) {
		return true
	}
	if !claudeTrustFrameClosingRule(lines[above]) {
		return false
	}
	// The box this edge closes: its rows up to its top edge, or to the top of
	// the pane when that edge has scrolled off.
	for i := above - 1; i >= 0 && claudeTrustBordered(lines[i]); i-- {
		if !rows[i].blank && !rows[i].selected &&
			(claudeComposerPromptLabel(rows[i].label) || strings.HasPrefix(rows[i].label, "> ")) {
			return true
		}
	}
	return false
}

// claudeLaunchDialogFooterInPlace is the pane with lines first to last, the
// pending-exit notice, read as footer: the footer on the notice's first line,
// at that line's indent, and the notice's other lines blank.
func claudeLaunchDialogFooterInPlace(lines []string, first, last int, footer string) visibleText {
	swapped := make([]string, len(lines))
	copy(swapped, lines)
	indent := lines[first][:len(lines[first])-len(strings.TrimLeft(lines[first], " "))]
	swapped[first] = indent + footer
	for i := first + 1; i <= last; i++ {
		swapped[i] = ""
	}
	return visibleText(strings.Join(swapped, "\n"))
}

// claudeFolderTrustReadToBottom reports whether a folder-trust reader that
// reads a dialog down to the pane's last content answers or holds screen: the
// legacy grammar, the reworded dialog with its question scrolled off
// (claudeFolderTrustHeadless and its siblings, which claudeFolderTrustDialogOf
// reports as unreadable when no question is on screen), the re-wrap matcher on
// a folder-trust dialog, or claudeLaunchDialogUnderGuide.
//
// The reworded grammar's own region is left out on purpose. It already holds
// a pending-exit notice under its options, so no such pane reaches this
// reader; and it reads a footer below a composer as a dialog row painted after
// the composer, which the notice in a composer's status row would become.
func claudeFolderTrustReadToBottom(screen visibleText) bool {
	if claudeLegacyTrustDialogOf(screen) != claudeLegacyTrustNone {
		return true
	}
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
	}
	if _, _, question := claudeFolderTrustQuestionRows(rows); !question &&
		claudeFolderTrustDialogOf(screen) == claudeFolderTrustUnreadable {
		return true
	}
	if match, held := claudeLaunchDialogRewrapOf(screen); held &&
		(match.kind == claudeLaunchDialogReworded || match.kind == claudeLaunchDialogLegacy) {
		return true
	}
	return claudeLaunchDialogUnderGuide(screen)
}

// refuseClaudeLaunchDialogRemnant reports, once per dialog rather than once
// per poll, a folder-trust dialog the remnant reader holds on.
func (t *TmuxSession) refuseClaudeLaunchDialogRemnant(kind claudeLaunchDialogRemnantKind) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	saw := "what a pane too narrow or too short leaves of its option rows, between its guide row and its footer"
	remedy := "make the pane larger, or answer the dialog in the session"
	switch kind {
	case claudeLaunchDialogRemnantFooterTail:
		saw = "nothing but the end of its footer"
	case claudeLaunchDialogRemnantPendingExit:
		saw = "an exit notice (\"Press Ctrl-C again to exit\") where its footer was, after an exit key pressed once in the pane"
		remedy = "it reads the dialog again once the notice clears; or answer the dialog in the session"
	}
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: the pane shows %s; af will not press a key it cannot aim; %s",
		claudeFolderTrustDialogName, t.sanitizedName, saw, remedy)
}
