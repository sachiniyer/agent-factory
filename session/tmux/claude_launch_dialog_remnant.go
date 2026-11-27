package tmux

import (
	"sort"
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
//     "No, exit" row alone, read the same way (claudeLaunchDialogUnderGuideOf).
//   - After one Ctrl-C the dialog's footer row reads "Press Ctrl-C again to
//     exit" for a moment. The reworded grammar holds that row
//     (claudeFolderTrustBroken); the legacy grammar read it as a row the
//     dialog never paints, and the pane as no dialog
//     (claudeLaunchDialogPendingExitOf).
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

// claudeLaunchDialogRemnant is what the remnant reader saw.
type claudeLaunchDialogRemnant struct {
	kind claudeLaunchDialogRemnantKind
	// folder: the pane proves the dialog is the folder-trust one. It shows
	// the dialog's guide row or the end of it, a footer only that dialog
	// paints ("Esc to exit"), or, behind an exit notice, rows a folder-trust
	// reader reads. Without one of those the pane shows only what the MCP
	// server dialog paints too: its footer is the reworded folder-trust
	// footer word for word (claudeMCPServerFooter), and a few characters of an
	// option row name no dialog. af holds either way; it names the dialog
	// only when the pane does (#7663).
	folder bool
}

// claudeLaunchDialogRemnantOf reports a launch dialog the pane shows a remnant
// of, in any of the shapes above.
func claudeLaunchDialogRemnantOf(screen visibleText) claudeLaunchDialogRemnantKind {
	return claudeLaunchDialogRemnantRead(screen).kind
}

// claudeLaunchDialogRemnantRead is claudeLaunchDialogRemnantOf with whether
// the pane proves which dialog it shows.
//
// The pane is split into rows once, and each reading reads those rows (#7663).
func claudeLaunchDialogRemnantRead(screen visibleText) claudeLaunchDialogRemnant {
	pane := claudeLaunchDialogPaneOf(screen)
	if held, guided := claudeLaunchDialogUnderGuideOf(pane); held {
		return claudeLaunchDialogRemnant{claudeLaunchDialogRemnantUnderGuide, guided || claudeLaunchDialogLegacyFooterLast(pane)}
	}
	if held, folder := claudeLaunchDialogPendingExitOf(pane); held {
		return claudeLaunchDialogRemnant{claudeLaunchDialogRemnantPendingExit, folder}
	}
	if claudeLaunchDialogFooterTail(pane) {
		return claudeLaunchDialogRemnant{claudeLaunchDialogRemnantFooterTail, claudeLaunchDialogLegacyFooterLast(pane)}
	}
	return claudeLaunchDialogRemnant{}
}

// claudeLaunchDialogLegacyFooterLast reports whether the pane's text ends
// with the footer only the folder-trust dialog paints, "Esc to exit", read
// back as far as the pane shows it and for three words at least.
func claudeLaunchDialogLegacyFooterLast(pane claudeLaunchDialogPane) bool {
	text := pane.text
	footer := claudeSquash(claudeLegacyTrustFooter)
	if len(text) > len(footer) {
		text = text[len(text)-len(footer):]
	}
	words := strings.Fields(claudeLegacyTrustFooter)
	least := strings.Join(words[len(words)-claudeLaunchDialogFooterTailMinWords:], "")
	return len(text) >= len(least) && strings.HasSuffix(footer, text)
}

// claudeLaunchDialogUnderGuideOf reports a pane whose content ends with the
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
//	            content, and no ❯ row — or a proper prefix of one that
//	            begins a row and ends the pane's text: the footer still
//	            painting, which the whole-footer pane also is until the
//	            frame lands whole (#7866, #7390's premise here)
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
//
// guided is whether the dialog's guide row, or the end of it, is on the pane.
// Only the folder-trust dialog paints that row.
//
// Its work is linear in the pane (#7663). Each row above the footer costs a
// prefix comparison; the option characters are compared only when they are no
// longer than an option block; a bracketed URL's end is looked up in an index
// built once; and the first guide row whose option characters read decides
// the rule check for every row above it.
func claudeLaunchDialogUnderGuideOf(pane claudeLaunchDialogPane) (held, guided bool) {
	end := -1
	for _, footer := range claudeLaunchDialogFooters {
		if strings.HasSuffix(pane.text, footer) {
			end = len(pane.text) - len(footer)
		}
	}
	if end > 0 {
		if held, guided := claudeLaunchDialogUnderGuideOver(pane, end); held {
			return held, guided
		}
	}
	// The footer may still be painting: the pane's last content is a proper
	// prefix of a footer that begins a row. The pane holds over the footer
	// whole, so it holds over every such prefix of it — #7390's premise —
	// read the same way: the same rows above it, the same guards (#7866).
	for content := len(pane.starts) - 1; content > 0; content-- {
		start := pane.text[pane.starts[content]:]
		prefix := false
		for _, footer := range claudeLaunchDialogFooters {
			if len(start) < len(footer) && strings.HasPrefix(footer, start) {
				prefix = true
			}
		}
		if !prefix {
			continue
		}
		if held, guided := claudeLaunchDialogUnderGuideOver(pane, pane.starts[content]); held {
			return held, guided
		}
	}
	return false, false
}

// claudeLaunchDialogUnderGuideOver reads the pane's head — the guide row, if
// any, and the option characters — above a footer that begins on a row at
// offset end and ends the pane's text, whether the footer is whole or only
// begun. The guards are the whole-footer pane's, unchanged: no ❯ row from the
// footer's row down, and the footer sits inside no box that closes under it.
func claudeLaunchDialogUnderGuideOver(pane claudeLaunchDialogPane, end int) (held, guided bool) {
	footerRow, ok := pane.rowStart(end)
	if !ok || claudeLaunchDialogAnySelected(pane, footerRow, len(pane.starts)) ||
		claudeLaunchDialogFooterInClosedBox(pane, footerRow) {
		return false, false
	}
	var closes []int // offsets of every ")" before the footer, built on first use
	closesBuilt := false
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
			if strings.HasPrefix(options, "(") {
				if !closesBuilt {
					closesBuilt = true
					for at := 0; at < end; at++ {
						if pane.text[at] == ')' {
							closes = append(closes, at)
						}
					}
				}
				from := pane.starts[guide] + len(row) + 1
				next := sort.SearchInts(closes, from)
				if next == len(closes) {
					continue
				}
				options = pane.text[closes[next]+1 : end]
			}
			if !claudeLaunchDialogOptionsRemnant(options) {
				continue
			}
			// A plain rule from this row down is one from every row above
			// it down too, and from the top of the pane: no reading is left.
			return !claudeLaunchDialogRuleFrom(pane, pane.index[guide]), true
		}
	}
	// No whole guide row: the option rows are the first content the pane
	// shows, or the end of the guide row the top of the pane cut is.
	if claudeLaunchDialogRuleFrom(pane, 0) {
		return false, false
	}
	head := strings.ReplaceAll(pane.text[:end], "\u200b", "")
	if claudeLaunchDialogOptionsRemnant(head) {
		return true, false
	}
	for _, row := range claudeLaunchDialogGuideRowsWhole {
		for cut := 1; cut < len(row); cut++ {
			if options, ok := strings.CutPrefix(head, row[cut:]); ok && claudeLaunchDialogOptionsRemnant(options) {
				return true, true
			}
		}
	}
	return false, false
}

// claudeLaunchDialogOptionsRemnant reports whether text is what a pane leaves
// of one of the dialogs' option blocks: not empty, and every character in it
// is one of that block's, no more of each than the block holds.
func claudeLaunchDialogOptionsRemnant(text string) bool {
	if text == "" {
		return false
	}
	for _, block := range claudeLaunchDialogOptionBlocks {
		// A block is ASCII, so text longer than it in bytes holds a
		// character the block lacks or more of one than it has.
		if len(text) <= len(block) && claudeCharactersOf(text, block) {
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
// Nor may the footer sit inside a box that closes under it
// (claudeLaunchDialogFooterInClosedBox).
func claudeLaunchDialogFooterTail(pane claudeLaunchDialogPane) bool {
	if !claudeLaunchDialogFooterEnd(pane.text) || claudeLaunchDialogFooterInClosedBox(pane, 0) ||
		claudeLaunchDialogAnySelected(pane, 0, len(pane.starts)) || claudeLaunchDialogRuleFrom(pane, 0) {
		return false
	}
	return true
}

// claudeLaunchDialogFooterInClosedBox reports whether the footer that begins
// on content row footerRow, the pane's last content, sits inside a box that
// closes under it: every line from the footer's first carries a box's sides,
// down to a bottom edge, with all of the footer above that edge. The pane's
// last painted line must be a bottom edge too, as #7652 asked, so this only
// ever excuses less than that did.
//
// No build paints its footer inside a box: 2.0.25, the one that boxes the
// dialog, paints the footer under the box
// (testdata/claude_2_0_25_legacy_folder_trust_dialog.ansi). A footer inside a
// box that closes under it is text in a boxed composer's draft whose prompt
// row has scrolled off, and a hold for a quoted draft is #7648's to decide.
//
// A bottom edge alone excuses nothing (#7663): a footer whose own rows carry
// no sides is not in the box that edge closes, and the pane still shows the
// dialog's remnant above a stray edge.
func claudeLaunchDialogFooterInClosedBox(pane claudeLaunchDialogPane, footerRow int) bool {
	if footerRow >= len(pane.index) {
		return false
	}
	last := len(pane.lines) - 1
	for last >= 0 && strings.TrimSpace(pane.lines[last]) == "" {
		last--
	}
	if last < 0 || !claudeTrustFrameClosingRule(pane.lines[last]) {
		return false
	}
	edge := pane.index[footerRow]
	for edge < last && !claudeTrustFrameClosingRule(pane.lines[edge]) {
		if !claudeTrustBordered(pane.lines[edge]) {
			return false
		}
		edge++
	}
	// The footer's last row is above the edge that closes its box.
	return edge > pane.index[footerRow] && pane.index[len(pane.index)-1] < edge
}

// claudeLaunchDialogPendingExitMaxKey bounds the key name in "Press <key>
// again to exit", squashed: "Ctrl-C" and "Ctrl-D" are the two Claude Code
// paints.
const claudeLaunchDialogPendingExitMaxKey = 12

// claudeLaunchDialogPendingExitMaxNotice is the longest notice the reader
// accepts, squashed: "Press", the longest key, "againtoexit".
const claudeLaunchDialogPendingExitMaxNotice = len("Press") + claudeLaunchDialogPendingExitMaxKey + len("againtoexit")

// claudeLaunchDialogPendingExitMaxTail bounds the footer tail a notice may
// leave below it, squashed: the longest footer a launch dialog paints is the
// picker's ("Space to select · Esc to reject all").
var claudeLaunchDialogPendingExitMaxTail = len(claudeSquash(claudeMCPServersFooter))

// claudeLaunchDialogPendingExitFooterTail reports whether text, squashed, is
// the end of a footer a launch dialog paints: the rows of it left below an
// exit notice that landed on its first row mid-paint (#7730 review, R1). The
// footer's own rows carry no cursor, so text read this way is at most a stale
// tail; the caller proves the dialog above the notice before it holds.
func claudeLaunchDialogPendingExitFooterTail(text string) bool {
	for _, footer := range claudeTrustFooters {
		if strings.HasSuffix(claudeSquash(footer), text) {
			return true
		}
	}
	return strings.HasSuffix(claudeSquash(claudeMCPServersFooter), text)
}

// claudeLaunchDialogPendingExitOf reports a launch dialog whose footer row reads
// "Press <key> again to exit": one exit key was pressed in the pane, and Claude
// Code shows the notice where the footer was until the press times out (#7275
// review).
//
// The notice ends the pane's content, on a row of its own or wrapped onto
// several — or it sits over the footer's first row with the footer's own tail
// still painted below it. Claude Code paints top-down (claude_folder_trust.go,
// claude_mcp_multiselect_painting.go), so a frame caught between the notice
// landing on the footer's first row and the tail under it being repainted is
// exactly as reachable as the notice-last one; the tail can only be the
// footer's, so the same footer-swap runs over the notice rows and the trailing
// rows together (#7730 review, R1). Rows below the notice that are not a
// suffix of a dialog's own footer leave the pane the way they found it.
//
// The pane with those rows read as a whole footer — each of the two the
// folder-trust dialogs paint (claudeTrustFooters), which the 2.1.322 single-server
// MCP dialog shares word for word (claudeMCPServerFooter) — must then be one a
// folder-trust reader that reads a dialog to the bottom of the pane answers or
// holds: the legacy grammar, the re-wrap matcher, the headless readers, or
// claudeLaunchDialogUnderGuideOf. So the notice proves nothing alone, and every
// guard those readers keep against a quoted dialog applies here unchanged.
//
// The multi-server picker (#7166) paints a footer of its own
// (claudeMCPServersFooter), so the footer-swap loop above never substitutes one.
// It is tried the same way after: the pane with that footer read in place of the
// notice holds whenever a reader that holds the picker behind its own footer
// holds it (claudeMCPServersReadToBottom) — the complete grammar and the
// unreadable hold (claudeMCPServersPickerOf), the tail reader when the header
// scrolled off or the pane wrapped the footer (#7463), and the painting reader
// under an earlier composer (#7504). The picker's footer is the only content it
// paints there, so the notice can only have replaced it once the picker was
// complete; the shapes the complete grammar cannot read still hold. af holds
// either way and names the picker not at all (folder is false): a hold is the
// same "hold, name it or not, send no key" shape the rest of the reader keeps.
//
// The notice need not be whole: a frame caught while Claude Code paints it
// leaves a proper prefix of it as the pane's last content ("Press Ctrl-C ag"),
// over a dialog whose footer is already gone. That pane holds like the frame
// with the footer erased: the notice's start is read the same way, the rows
// above proven with a footer swapped in, and the same holds named or not
// (#7725).
//
// A working agent's composer paints the same notice in its status row. That
// row is straight under the composer's closing line, a plain rule or its box's
// bottom edge, and a dialog paints neither straight above its footer except the
// bottom edge of its own box, which holds no prompt row. So a notice straight
// under a composer's closing line is the composer's, and the pane stays clear.
//
// folder is whether the rows above the notice prove the dialog is the
// folder-trust one.
//
// For each row the notice could end on — the pane's last row, or a row above
// the end of a dialog's own footer still painted below it — it reads up for
// the notice, and stops once the rows read are longer than any notice it
// accepts (claudeLaunchDialogPendingExitMaxNotice): rows above only make the
// text longer. The tail walk stops once the rows below read longer than any
// footer (claudeLaunchDialogPendingExitMaxTail). So both walks are bounded,
// and the reading is linear in the pane (#7663).
func claudeLaunchDialogPendingExitOf(pane claudeLaunchDialogPane) (held, folder bool) {
	lines, rows := pane.lines, pane.rows
	if len(pane.index) == 0 {
		return false, false
	}
	last := pane.index[len(pane.index)-1]
	tail := ""
	for end := last; end >= 0; end-- {
		if end < last {
			// tail is the rows below end, squashed. It is the dialog's own
			// footer only while a footer ends with it: the picker's footer is
			// the longest any launch dialog paints, and tail only grows as
			// end walks up.
			tail = claudeSquash(rows[end+1].label) + tail
			if len(tail) > claudeLaunchDialogPendingExitMaxTail {
				break
			}
			if !claudeLaunchDialogPendingExitFooterTail(tail) {
				continue
			}
		}
		notice := ""
		for first := end; first >= 0 && !rows[first].blank && !rows[first].selected; first-- {
			notice = claudeSquash(rows[first].label) + notice
			if len(notice) > claudeLaunchDialogPendingExitMaxNotice {
				break
			}
			if !claudeLaunchDialogPendingExitNotice(notice) && !claudeLaunchDialogPendingExitNoticeStart(notice) {
				continue
			}
			if claudeLaunchDialogNoticeUnderComposer(lines, rows, first) || !claudeLaunchDialogPaintedAbove(rows, first) {
				return false, false
			}
			for _, footer := range claudeTrustFooters {
				if tail != "" && !strings.HasSuffix(claudeSquash(footer), tail) {
					continue
				}
				if held, folder := claudeFolderTrustReadToBottom(claudeLaunchDialogFooterInPlace(pane, first, last, footer)); held {
					return true, folder
				}
			}
			// The multi-server picker's footer is its own, not shared with
			// the folder-trust dialogs, so the swap loop above never
			// substitutes it. Try it the same way: the pane with the picker's
			// footer read where the notice is holds whenever a reader that
			// holds the picker behind its own footer holds it. folder is
			// false: this is the MCP picker, not the folder-trust dialog,
			// and af names it not at all.
			if tail == "" || strings.HasSuffix(claudeSquash(claudeMCPServersFooter), tail) {
				if screen, _ := claudeLaunchDialogFooterInPlace(pane, first, last, claudeMCPServersFooter); claudeMCPServersReadToBottom(screen) {
					return true, false
				}
			}
			return false, false
		}
	}
	return false, false
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

// claudeLaunchDialogPendingExitNoticeStart reports whether text, squashed, is
// a proper prefix of "Press <key> again to exit": the notice while Claude Code
// is still painting it, where the dialog's footer was (#7725). The key is
// bounded the same way as the whole notice's
// (claudeLaunchDialogPendingExitMaxKey), so the bound on the rows the walk
// reads is unchanged.
func claudeLaunchDialogPendingExitNoticeStart(text string) bool {
	if !strings.HasPrefix(text, "Press") {
		// A prefix shorter than "Press" itself: "P" is as much of the notice
		// as "Press Ctrl-C ag".
		return strings.HasPrefix("Press", text)
	}
	// rest is key characters, then the start of "againtoexit". Up to the
	// longest key it is all key characters; longer, it must already spell
	// the start of "againtoexit".
	rest := text[len("Press"):]
	for key := 0; key <= claudeLaunchDialogPendingExitMaxKey && key < len(rest); key++ {
		if strings.HasPrefix("againtoexit", rest[key:]) {
			return true
		}
	}
	return len(rest) <= claudeLaunchDialogPendingExitMaxKey
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
// at that line's indent, and the notice's other lines blank. It returns that
// screen and its rows, which are the pane's own down to the notice: only the
// notice's lines are read again.
func claudeLaunchDialogFooterInPlace(pane claudeLaunchDialogPane, first, last int, footer string) (visibleText, claudeLaunchDialogPane) {
	swapped := claudeLaunchDialogPane{
		lines: make([]string, len(pane.lines)),
		rows:  make([]claudeTrustRow, len(pane.rows)),
	}
	copy(swapped.lines, pane.lines)
	copy(swapped.rows, pane.rows)
	line := pane.lines[first]
	swapped.lines[first] = line[:len(line)-len(strings.TrimLeft(line, " "))] + footer
	for i := first + 1; i <= last; i++ {
		swapped.lines[i] = ""
	}
	for i := first; i <= last; i++ {
		swapped.rows[i] = claudeTrustRowOf(swapped.lines[i])
	}
	// The content rows above the notice, then the lines just read.
	above := sort.SearchInts(pane.index, first)
	swapped.starts = append(swapped.starts, pane.starts[:above]...)
	swapped.index = append(swapped.index, pane.index[:above]...)
	text := pane.text
	if above < len(pane.starts) {
		text = text[:pane.starts[above]]
	}
	for i := first; i <= last; i++ {
		if swapped.rows[i].blank {
			continue
		}
		swapped.starts = append(swapped.starts, len(text))
		swapped.index = append(swapped.index, i)
		text += claudeSquash(swapped.rows[i].label)
	}
	swapped.text = text
	return visibleText(strings.Join(swapped.lines, "\n")), swapped
}

// claudeFolderTrustReadToBottom reports whether a folder-trust reader that
// reads a dialog down to the pane's last content answers or holds screen: the
// legacy grammar, the reworded dialog with its question scrolled off
// (claudeFolderTrustHeadless and its siblings, which claudeFolderTrustDialogOf
// reports as unreadable when no question is on screen), the re-wrap matcher on
// a folder-trust dialog or the 2.1.322 MCP-server dialog whose footer it shares
// (the matcher names the kind, and folder is false for the MCP-server kind), or
// claudeLaunchDialogUnderGuideOf.
//
// The reworded grammar's own region is left out on purpose. It already holds
// a pending-exit notice under its options, so no such pane reaches this
// reader; and it reads a footer below a composer as a dialog row painted after
// the composer, which the notice in a composer's status row would become.
//
// pane is screen's rows (claudeLaunchDialogPaneOf), which the caller has.
// folder is whether what it read proves the dialog is the folder-trust one.
// Every reader but the last reads rows only that dialog paints. The last,
// claudeLaunchDialogUnderGuideOf, proves it by the guide row alone: the footer
// on this screen is one the caller put there.
func claudeFolderTrustReadToBottom(screen visibleText, pane claudeLaunchDialogPane) (held, folder bool) {
	if claudeLegacyTrustDialogOf(screen) != claudeLegacyTrustNone {
		return true, true
	}
	if _, _, question := claudeFolderTrustQuestionRows(pane.rows); !question &&
		claudeFolderTrustDialogOf(screen) == claudeFolderTrustUnreadable {
		return true, true
	}
	if match, held := claudeLaunchDialogRewrapOf(screen); held &&
		(match.kind == claudeLaunchDialogReworded || match.kind == claudeLaunchDialogLegacy ||
			match.kind == claudeLaunchDialogMCPServer) {
		// The MCP-server dialog's footer (claudeMCPServerFooter) is the
		// reworded folder-trust footer word for word, so the same
		// footer-swap that proves a folder-trust dialog behind an exit
		// notice proves an MCP-server dialog here: the rewrap matcher
		// reads that dialog's own option labels above the shared footer.
		// af holds either way, and the #7663 naming policy reports the
		// remnant unnamed (DialogHoldOther) unless the pane proves the
		// folder-trust one: the matcher proves the MCP-server dialog, not
		// the folder-trust one, so folder is false for that kind. Holding
		// is all af sends, Enter on the live modal would confirm whichever
		// row owns the cursor, including code-executing "Use" rows.
		return true, match.kind != claudeLaunchDialogMCPServer
	}
	return claudeLaunchDialogUnderGuideOf(pane)
}

// claudeMCPServersReadToBottom reports whether a reader that holds the
// multi-server picker when its own footer is the pane's last content answers
// or holds screen (#7725): the picker's grammar and its unreadable hold
// (claudeMCPServersPickerOf), the tail reader for a picker whose header
// scrolled off or whose footer the pane wrapped (claudeMCPServersTailOf,
// #7463), and the painting reader for one under a composer an earlier Claude
// left above it (claudeMCPServersPaintingOf, #7504) — the same readers, in the
// same order, the gate asks after the picker's own clauses. A whole footer is
// never still being painted, so the last of these cannot fire on a screen
// claudeLaunchDialogFooterInPlace produces; it is asked so the set is every
// reader the gate has for the picker.
func claudeMCPServersReadToBottom(screen visibleText) bool {
	return claudeMCPServersPickerOf(screen) != claudeMCPServersNone ||
		claudeMCPServersTailOf(screen) ||
		claudeMCPServersPaintingOf(screen)
}

// refuseClaudeLaunchDialogRemnant reports, once per dialog rather than once
// per poll, a dialog the remnant reader holds on. It names the folder-trust
// dialog only when the pane proves it is that one, and the launch gate's
// dialogs otherwise.
func (t *TmuxSession) refuseClaudeLaunchDialogRemnant(remnant claudeLaunchDialogRemnant) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	name := claudeLaunchGateDialogName
	saw := "what a pane too narrow or too short leaves of a dialog's option rows, over its footer"
	if remnant.folder {
		name = claudeFolderTrustDialogName
		saw = "what a pane too narrow or too short leaves of its option rows, between its guide row and its footer"
	}
	remedy := "make the pane larger, or answer the dialog in the session"
	switch remnant.kind {
	case claudeLaunchDialogRemnantFooterTail:
		saw = "nothing but the end of its footer"
	case claudeLaunchDialogRemnantPendingExit:
		saw = "an exit notice (\"Press Ctrl-C again to exit\") where its footer was, after an exit key pressed once in the pane"
		remedy = "it reads the dialog again once the notice clears; or answer the dialog in the session"
	}
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: the pane shows %s; af will not press a key it cannot aim; %s",
		name, t.sanitizedName, saw, remedy)
}
