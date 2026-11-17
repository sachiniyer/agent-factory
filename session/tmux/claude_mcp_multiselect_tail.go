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
//     whole footer, "Space to select · Esc to reject all", on one row or on
//     several, read with its whitespace removed so that no wrap point matters
//     (#7346), and with no ❯ on any of its rows;
//   - the content straight above the footer, past blank rows and frame chrome,
//     is the "Enable selected" row, read the same way, or as much of it as the
//     pane's top edge leaves; or the footer is the first content the pane
//     shows.
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
		return false
	}
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
