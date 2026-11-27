package tmux

import "strings"

// claudeMCPServersTailOf reports the 2.1.322 multi-server picker (#7166) from
// its own trailing rows alone (#7463): the last content in the pane is its
// footer, and the content straight above the footer, if the pane shows any, is
// its "Enable selected" row.
//
// claudeMCPServersPickerOf reads the picker from its header down. A pane too
// short for the whole picker scrolls the header off (at 80 columns, a picker
// with five server rows on screen needs 13), and a pane narrower than the
// footer wraps it. Either way neither its grammar nor its unreadable hold
// matched, the pane read clear, and the create typed its prompt and pressed
// Enter into a picker that has every server pre-checked.
//
// CheckAndHandleTrustPrompt asks it after every clause that answers a dialog
// has declined the frame, and asks only claudeMCPServersPaintingOf (#7504)
// after it. It sends no key, so it turns a clear pane into a hold and changes
// nothing else.
//
// It holds when both of these are true:
//
//   - the last content in the pane, frame rules counted as content, is the
//     footer, "Space to select · Esc to reject all": whole, on one row or on
//     several, or a proper prefix of it that begins a row — the same footer
//     at an earlier paint instant (#7390's premise, #7870) — read with its
//     whitespace removed so that no wrap point matters (#7346), and with no
//     ❯ on any of its rows;
//   - the content straight above the footer, past blank rows and frame chrome,
//     is the "Enable selected" row, read the same way, or as much of it as the
//     pane's top edge leaves; or the footer is the first content the pane
//     shows.
//
// A proper footer prefix is the footer still painting: Claude Code draws the
// picker's last row left to right, so a capture can land before it is whole
// with the picker's header already scrolled off. Until #7870 such a pane read
// clear — the readers above this one wanted the header or the footer whole —
// and the create typed its prompt into the picker, where Enter confirmed
// whichever row owned the cursor, pre-checked servers that run code among
// them.
//
// Nothing above the "Enable selected" row is asked. A pane tall enough for the
// header has the grammar read the picker; a shorter one shows some of the
// server rows, or none, and a long server name wraps into rows that are no
// checkbox (the long-name capture). A row above is evidence for the picker or
// no evidence at all, never against it: a composer that a relaunch in the same
// pane left above a live picker explains only its own rows (#7388). Below the
// footer, nothing may be painted. A working agent paints its composer, framed
// by rules, under its output, so a picker it quotes has the composer below the
// footer and reads clear.
//
// Another picker that ends in the same two rows holds too. af cannot tell it
// from this one, and a prompt typed into either is lost. The cost of the hold
// is the create's trust budget running out, which is loud and can be retried.
func claudeMCPServersTailOf(screen visibleText) bool {
	lines := screen.lines()
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	footer, whole := claudeRowsSpelling(lines, last, claudeSquash(claudeMCPServersFooter), false)
	if !whole {
		// The footer may still be painting: the pane's last content is a
		// proper prefix of it that begins a row. A pane that holds over the
		// footer whole holds over every such prefix — the same footer at an
		// earlier paint instant — so the rows above it are read the same
		// way (#7870).
		footer = claudeMCPServersFooterPrefixStart(lines, last)
		if footer < 0 {
			return false
		}
	}
	return claudeMCPServersTailAbove(lines, footer)
}

// claudeMCPServersTailAbove reads the pane above the picker's footer — whole,
// or begun as far as row footer — the way the whole-footer pane is read: the
// content straight above the footer, past blank rows and frame chrome, is the
// "Enable selected" row, or as much of it as the pane's top edge leaves; or
// the footer is the first content the pane shows.
func claudeMCPServersTailAbove(lines []string, footer int) bool {
	submit := footer - 1
	for submit >= 0 && claudeTrustRowOf(lines[submit]).blank {
		submit--
	}
	if submit < 0 {
		// The footer is the first content the pane shows: a pane too short
		// for even the submit row.
		return true
	}
	top, whole := claudeRowsSpelling(lines, submit, claudeSquash(claudeMCPServersSubmitLabel), true)
	// A submit row the pane's top edge cuts into: the rows from the top down
	// to the footer spell its end.
	return whole || top == 0
}

// claudeMCPServersFooterPrefixStart returns the row the picker's footer
// begins on when the pane's last content, through row last, is a proper
// prefix of it that begins a row: the footer still painting. -1 when the
// rows are none of that. Read with whitespace removed, as claudeRowsSpelling
// reads the whole footer: the footer's own rows carry no cursor and no blank
// row, so a selected or a blank row ends them, and text longer than the
// footer can no longer be its prefix.
func claudeMCPServersFooterPrefixStart(lines []string, last int) int {
	footer := claudeSquash(claudeMCPServersFooter)
	text, start := "", -1
	for i := last; i >= 0; i-- {
		row := claudeTrustRowOf(lines[i])
		if row.blank || row.selected {
			break
		}
		text = claudeSquash(row.label) + text
		if len(text) >= len(footer) {
			break
		}
		if strings.HasPrefix(footer, text) {
			start = i
		}
	}
	return start
}

// claudeRowsSpelling reads the rows that end at row end, upward, as one text
// with their whitespace removed, and reports whether they spell want exactly;
// start is the topmost row read. It stops at a blank row or frame chrome, at
// a row that would make the text no longer end want, and at the top of the
// pane, where it returns start 0 if what it read so far ends want. A row with
// the ❯ cursor is read by its label only when cursor allows one.
func claudeRowsSpelling(lines []string, end int, want string, cursor bool) (start int, whole bool) {
	read := ""
	for i := end; i >= 0; i-- {
		row := claudeTrustRowOf(lines[i])
		if row.blank || (row.selected && !cursor) {
			return i + 1, false
		}
		read = claudeSquash(row.label) + read
		if read == want {
			return i, true
		}
		if !strings.HasSuffix(want, read) {
			return i + 1, false
		}
	}
	return 0, false
}
