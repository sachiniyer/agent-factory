package tmux

import "strings"

// claudeMCPServersTailVerdict is what the pane's own trailing rows say of the
// picker's footer.
type claudeMCPServersTailVerdict int

const (
	// claudeMCPServersTailNone: the pane's trailing rows are not the picker's
	// footer over its "Enable selected" row.
	claudeMCPServersTailNone claudeMCPServersTailVerdict = iota
	// claudeMCPServersTailWhole: the pane's last content is the whole footer
	// under its "Enable selected" row — the hold #7463 added, which af
	// reports as a refusal, once, the way it reports the picker's other
	// unreadable shapes.
	claudeMCPServersTailWhole
	// claudeMCPServersTailBegun: the pane's last content is a proper prefix
	// of the footer that begins a row — the same footer at an earlier paint
	// instant (#7870). Held silently, as painting: the refusal log and the
	// unreadable hold kind stay the whole-footer read's, the same hold
	// claudeMCPServersPaintingOf gives a begun row it can see (#7872 review,
	// R2).
	claudeMCPServersTailBegun
	// claudeMCPServersTailSubmit: the pane's last content is the "Enable
	// selected" row itself — whole, a proper prefix of it that begins the
	// row, the row painted only as far as its ❯ cursor, or as much of it as
	// the pane's top edge leaves — one paint instant before the footer
	// begins (#7873, #7876 review R1). Held silently, as painting,
	// the way a begun footer is.
	claudeMCPServersTailSubmit
)

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
// after it — except its submit verdict, which it acts on after the remnant
// read so a frame a clause already holds keeps that clause's own kind and
// refusal (#7876 review, R2). It sends no key, so it turns a clear pane
// into a hold and changes nothing else.
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
// And one paint instant before the footer begins (#7873) it holds when the
// pane's last content is the "Enable selected" row itself: whole, a proper
// prefix of it that begins the row, the row painted only as far as its ❯
// cursor, or as much of it as the pane's top edge leaves — read squashed
// and behind an optional ❯ the way the begun-footer read reads the footer —
// with the content straight above it a row the picker paints there, or
// nothing at all. The guard is the begun-footer read's: nothing ordinary is
// painted below it.
//
// A proper footer prefix is the footer still painting: Claude Code draws the
// picker's last row left to right, so a capture can land before it is whole
// with the picker's header already scrolled off. Until #7870 such a pane read
// clear — the readers above this one wanted the header or the footer whole —
// and the create typed its prompt into the picker, where Enter confirmed
// whichever row owned the cursor, pre-checked servers that run code among
// them. The submit row alone is the same frame one instant earlier: the
// paint has not reached the footer, and until #7873 it read clear too.
//
// Nothing above the "Enable selected" row is asked of a footer read. A pane
// tall enough for the header has the grammar read the picker; a shorter one
// shows some of the server rows, or none, and a long server name wraps into
// rows that are no checkbox (the long-name capture). A row above is evidence
// for the picker or no evidence at all, never against it: a composer that a
// relaunch in the same pane left above a live picker explains only its own
// rows (#7388). Below the footer, nothing may be painted. A working agent
// paints its composer, framed by rules, under its output, so a picker it
// quotes has the composer below the footer and reads clear.
//
// Another picker that ends in the same two rows holds too. af cannot tell it
// from this one, and a prompt typed into either is lost. The cost of the hold
// is the create's trust budget running out, which is loud and can be retried.
func claudeMCPServersTailOf(screen visibleText) claudeMCPServersTailVerdict {
	lines := screen.lines()
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	footer, whole := claudeRowsSpelling(lines, last, claudeSquash(claudeMCPServersFooter), false)
	if whole {
		if claudeMCPServersTailAbove(lines, footer) {
			return claudeMCPServersTailWhole
		}
		return claudeMCPServersTailNone
	}
	// The footer may still be painting: the pane's last content is a proper
	// prefix of it that begins a row. A pane that holds over the footer
	// whole holds over every such prefix — the same footer at an earlier
	// paint instant — so the rows above it are read the same way (#7870).
	footer = claudeMCPServersFooterPrefixStart(lines, last)
	if footer >= 0 && claudeMCPServersTailAbove(lines, footer) {
		return claudeMCPServersTailBegun
	}
	if last < 0 {
		return claudeMCPServersTailNone
	}
	// One paint instant earlier still, the footer row has not begun: the
	// pane's last content is the "Enable selected" row itself — whole, a
	// proper prefix of it that begins the row, or the row painted only as
	// far as its cursor — read squashed and behind an optional ❯ the way
	// the begun-footer read reads the footer, with the same guard that
	// nothing ordinary is painted below it. A pane that holds over a begun
	// footer holds over the submit row that was its last content the
	// instant before (#7873).
	submit, whole := claudeRowsSpelling(lines, last, claudeSquash(claudeMCPServersSubmitLabel), true)
	switch {
	case whole:
		if claudeMCPServersSubmitAbove(lines, submit) {
			return claudeMCPServersTailSubmit
		}
	case submit == 0 && claudeSquash(claudeTrustRowOf(lines[last]).label) != "":
		// The pane's top edge cut into the submit row: its rows down to the
		// last content spell its end — as much of it as the pane shows. The
		// last row must carry part of the label: a pane of bare ❯ rows
		// spells nothing.
		return claudeMCPServersTailSubmit
	default:
		submit = claudeMCPServersSubmitPrefixStart(lines, last)
		if submit >= 0 && claudeMCPServersSubmitAbove(lines, submit) {
			return claudeMCPServersTailSubmit
		}
		// Or the submit row begun only as far as its cursor: a bare ❯ row
		// as the pane's last content.
		if claudeMCPServersCursorTail(lines, last) {
			return claudeMCPServersTailSubmit
		}
	}
	return claudeMCPServersTailNone
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

// claudeMCPServersSubmitAbove reads the pane above the picker's "Enable
// selected" row — whole, or begun as far as row submit — the way
// claudeMCPServersTailAbove reads the pane above the footer: the content
// straight above the submit row, past blank rows and frame chrome, is a row
// the picker paints there, or the submit row is the first content the pane
// shows.
//
// The picker's own rows above its submit row are its checkbox rows and, when
// a long server name wraps, the wrapped rest of a name — a row of plain
// text, which a top cut inside the name can leave as the only server row on
// screen (the long-name capture). Every unselected row straight above counts
// as one of those; the row the picker never paints between its servers and
// its submit row is a selected row that is not a checkbox: a bare ❯, or the
// cursor row of anything else (#7873).
func claudeMCPServersSubmitAbove(lines []string, submit int) bool {
	above := submit - 1
	for above >= 0 && claudeTrustRowOf(lines[above]).blank {
		above--
	}
	if above < 0 {
		// The submit row is the first content the pane shows: a pane too
		// short for even one server row.
		return true
	}
	row := claudeTrustRowOf(lines[above])
	return !row.selected || claudeMCPServerCheckboxRow(row.label)
}

// claudeMCPServersSubmitPrefixStart returns the row the picker's "Enable
// selected" row begins on when the pane's last content, through row last, is
// a proper prefix of it that begins a row: the submit row still painting.
// -1 when the rows are none of that. Read with whitespace removed, as
// claudeMCPServersFooterPrefixStart reads the footer's — except that the
// submit row can carry the ❯ cursor where the footer never does, so a
// selected row's label counts toward the prefix the way claudeRowsSpelling
// reads it with the cursor kept. A row with no label at all — a bare ❯
// prompt — ends the row the way a blank does.
func claudeMCPServersSubmitPrefixStart(lines []string, last int) int {
	submit := claudeSquash(claudeMCPServersSubmitLabel)
	text, start := "", -1
	for i := last; i >= 0; i-- {
		row := claudeTrustRowOf(lines[i])
		label := claudeSquash(row.label)
		if row.blank || label == "" {
			break
		}
		text = label + text
		if len(text) >= len(submit) {
			break
		}
		if strings.HasPrefix(submit, text) {
			start = i
		}
	}
	return start
}

// claudeMCPServersCursorTail reports whether the pane's last content, at
// last, is the picker's "Enable selected" row painted only as far as its
// cursor: a bare ❯ row directly under a row the picker paints there — a
// checkbox row, or the wrapped rest of a server name that a checkbox row in
// the pane owns (#7876 review, R1). The row the picker never paints between
// them is everything else: a rule or a blank row — a composer's edge over a
// live prompt — ends the picker, and a bare ❯ with nothing but unmarked
// text above it, or nothing at all, is only a prompt.
func claudeMCPServersCursorTail(lines []string, last int) bool {
	row := claudeTrustRowOf(lines[last])
	if !row.selected || claudeSquash(row.label) != "" {
		return false
	}
	for above := last - 1; above >= 0; above-- {
		row = claudeTrustRowOf(lines[above])
		if claudeMCPServerCheckboxRow(row.label) {
			return true
		}
		if row.blank || row.selected {
			return false
		}
	}
	return false
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
