package tmux

import (
	"errors"
	"strings"
)

// claudeFolderTrustQuestion is the reworded folder-trust dialog's question.
// Claude Code paints it inside a sentence ("Quick safety check: Is this a
// project you created or one you trust? …"), so a row CONTAINS it rather than
// being it, and a pane too narrow for that sentence wraps it across rows.
const claudeFolderTrustQuestion = "Is this a project you created or one you trust"

// claudeFolderTrustExitLabel is the dialog's other option, and its
// preselection, on Claude Code 2.1.257 and 2.1.258 (#3579).
const claudeFolderTrustExitLabel = "No, exit"

// claudeFolderTrustFooter is the dialog's footer in full. A row that starts
// claudeTrustAffordancePrefix is the footer; the full text is what a footer a
// narrow pane wrapped onto two rows must join back into.
const claudeFolderTrustFooter = "Enter to confirm · Esc to cancel"

// claudeFolderTrustMinLabelStart is the shortest first row a narrow pane can
// leave of the affirmative label that is still read as that option: "Yes, I".
// "Yes," alone is too common a row to mean the option.
const claudeFolderTrustMinLabelStart = len("Yes, I")

// errClaudeFolderTrustUnreadable is why af refuses a folder-trust dialog it can
// see but cannot read as rows.
var errClaudeFolderTrustUnreadable = errors.New(
	"its rows do not read as the dialog af knows (a pane too narrow for them wraps them; widen the pane or answer the dialog in the session)")

// claudeFolderTrustVerdict is what one capture says about the dialog.
type claudeFolderTrustVerdict int

const (
	// claudeFolderTrustNone: the pane is not showing the dialog.
	claudeFolderTrustNone claudeFolderTrustVerdict = iota
	// claudeFolderTrustPartial: a proper prefix of the dialog; Claude Code is
	// still painting it. Hold: send nothing, and do not report the pane clear.
	claudeFolderTrustPartial
	// claudeFolderTrustUnreadable: the whole dialog, with a row af reads by
	// wrapped onto two. Hold without a key, and say why once.
	claudeFolderTrustUnreadable
	// claudeFolderTrustComplete: the whole dialog, every row af reads by on a
	// row of its own. parseClaudeFolderTrustDialog decides whether af can aim
	// at it.
	claudeFolderTrustComplete
)

// claudeFolderTrustDialogOf decides whether Claude Code's reworded folder-trust
// dialog owns the pane. It runs on the daemon's continuous poll against
// arbitrary agent output, and a key typed into a working composer cannot be
// taken back, so it decides from the ONE property claudeLegacyTrustDialogOf,
// claudeMCPServerDialogOf and claudeMCPServersPickerOf decide from (#7271):
//
// The dialog owns the pane only when its question opens the pane's trailing
// region and every row from there to the bottom of the pane is one the dialog
// paints, in the dialog's order:
//
//	question      rows, none of them a ❯ row, holding claudeFolderTrustQuestion
//	body rows     plain text: no ❯, no frame rule, no footer
//	option block  one unbroken run of rows holding the "Yes, I trust this
//	              folder" row, begun by the first option row below the body
//	footer        "Enter to confirm…", then only blank rows and frame chrome
//
// and no ❯ row above the question: a launch modal owns the pane from the top,
// and a composer anywhere above it paints one.
//
// Partial is a proper prefix of that. Claude Code paints top-down, so a capture
// can hold the question and body with no options yet, some options, or all of
// them with no footer — its last row possibly cut short mid-paint.
//
// Unreadable is the whole dialog with a row af reads by wrapped: the question,
// the affirmative label or the footer. Claude Code's renderer wraps its own
// rows in a pane too narrow for them — a phone client's resize is enough
// (ResizeWindow applies the last client's size) — and `capture-pane -J` joins
// only the terminal's wraps, not those. The question sits mid-sentence, so it
// wraps well before the options or the footer do.
//
// Anything else is no dialog — in particular the dialog's text with something
// it never paints below it: the agent's composer (framed by rules, led by ❯),
// more output, a draft. A working agent paints its composer below its output,
// so a dialog it quotes — a diff or a grep of this file, a review — reads as
// no dialog, and the create delivers its prompt to the agent. #3587 held such a
// pane instead, refusing the quote until the create failed.
func claudeFolderTrustDialogOf(screen visibleText) claudeFolderTrustVerdict {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	lastPainted, firstCursor := -1, -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if !rows[i].blank {
			lastPainted = i
		}
		if rows[i].selected && firstCursor < 0 {
			firstCursor = i
		}
	}
	first, last, ok := claudeFolderTrustQuestionRows(rows)
	if !ok || (firstCursor >= 0 && firstCursor < first) {
		return claudeFolderTrustNone
	}
	wrapped := first != last

	const (
		inBody = iota
		inOptions
		afterOptions
		inFooter
		afterFooter
	)
	phase, options, footer := inBody, -1, -1
	for i := last + 1; i < len(rows); i++ {
		row, rule := rows[i], claudeTrustFrameRule(lines[i])
		switch phase {
		case inBody:
			switch {
			case rule || claudeTrustIsFooter(row.label):
				return claudeFolderTrustNone
			case row.blank:
			case claudeFolderTrustOptionStarts(rows, i, i == lastPainted):
				phase, options = inOptions, i
			case row.selected:
				// A ❯ row that is none of this dialog's options is a composer.
				return claudeFolderTrustNone
			}
		case inOptions:
			switch {
			case rule:
				// The dialog paints no rule between its options and its
				// footer; a composer does.
				return claudeFolderTrustNone
			case row.blank:
				phase = afterOptions
			case claudeTrustIsFooter(row.label):
				phase, footer = inFooter, i
			}
		case afterOptions:
			switch {
			case rule:
				return claudeFolderTrustNone
			case row.blank:
			case claudeTrustIsFooter(row.label):
				phase, footer = inFooter, i
			case i == lastPainted && strings.HasPrefix(claudeTrustAffordancePrefix, row.label):
				// The footer, cut short mid-paint.
				return claudeFolderTrustPartial
			default:
				return claudeFolderTrustNone
			}
		case inFooter:
			// Content straight below the footer row is the rest of a footer the
			// pane wrapped; claudeFolderTrustFooterRead checks it.
			if row.blank {
				phase = afterFooter
			}
		case afterFooter:
			if !row.blank && !rule {
				return claudeFolderTrustNone
			}
		}
	}
	if phase < inFooter {
		return claudeFolderTrustPartial
	}

	label, ok := claudeFolderTrustLabelRead(rows, options, last, footer)
	if !ok {
		// A footer under options, but not this dialog's options.
		return claudeFolderTrustNone
	}
	whole, ok := claudeFolderTrustFooterRead(rows, footer)
	if !ok {
		return claudeFolderTrustNone
	}
	if wrapped || !label || !whole {
		return claudeFolderTrustUnreadable
	}
	return claudeFolderTrustComplete
}

// claudeFolderTrustQuestionRows finds the rows the question is painted on, the
// LAST place it appears, so the region it opens is the one at the bottom of the
// pane. A pane wide enough paints it on one row (first == last). A narrower one
// wraps it across consecutive rows, which read back as the question once their
// labels are joined by single spaces — the renderer breaks a sentence only at
// its spaces. None of those rows may be a ❯ row: a composer draft that holds
// the question is not the dialog.
func claudeFolderTrustQuestionRows(rows []claudeTrustRow) (first, last int, ok bool) {
	end := len(rows) - 1
	for end >= 0 {
		if rows[end].blank {
			end--
			continue
		}
		start := end
		for start > 0 && !rows[start-1].blank {
			start--
		}
		// The block's labels joined by single spaces, and where each starts.
		var joined strings.Builder
		offsets := make([]int, 0, end-start+1)
		for i := start; i <= end; i++ {
			if i > start {
				joined.WriteByte(' ')
			}
			offsets = append(offsets, joined.Len())
			joined.WriteString(rows[i].label)
		}
		text := joined.String()
		for upTo := len(text); upTo > 0; {
			at := strings.LastIndex(text[:upTo], claudeFolderTrustQuestion)
			if at < 0 {
				break
			}
			first, last = start+rowAtOffset(offsets, at), start+rowAtOffset(offsets, at+len(claudeFolderTrustQuestion)-1)
			cursor := false
			for i := first; i <= last; i++ {
				cursor = cursor || rows[i].selected
			}
			if !cursor {
				return first, last, true
			}
			upTo = at
		}
		end = start - 1
	}
	return 0, 0, false
}

// rowAtOffset is the index of the row whose label holds byte offset at of the
// joined block, given where each row's label starts.
func rowAtOffset(offsets []int, at int) int {
	row := 0
	for i, start := range offsets {
		if start <= at {
			row = i
		}
	}
	return row
}

// claudeFolderTrustOptionStarts reports whether row i, below the body, begins
// the option block. It does when it is one of the dialog's own option rows —
// the affirmative or the exit row, whole or as the first row a narrow pane
// leaves of it. A ❯ row begins it too when the unbroken run it sits in holds
// one of those rows, whatever its own label: af navigates by the affirmative
// label alone, so a build that adds or renames the other options must still
// read as the dialog. And so does a ❯ row that is the last row painted so far
// and is cut short of an option label. Any other ❯ row is a composer.
func claudeFolderTrustOptionStarts(rows []claudeTrustRow, i int, lastPainted bool) bool {
	if claudeFolderTrustOptionLabel(rows[i].label) {
		return true
	}
	if !rows[i].selected {
		return false
	}
	if label := rows[i].label; lastPainted &&
		(strings.HasPrefix(claudeTrustAffirmativeLabel, label) || strings.HasPrefix(claudeFolderTrustExitLabel, label)) {
		return true
	}
	from, to := claudeTrustRunBounds(rows, i)
	for j := from; j <= to; j++ {
		if claudeFolderTrustOptionLabel(rows[j].label) {
			return true
		}
	}
	return false
}

// claudeFolderTrustOptionLabel reports whether label is one of the dialog's own
// option rows, or the first row a narrow pane leaves of the affirmative one.
func claudeFolderTrustOptionLabel(label string) bool {
	return label == claudeTrustAffirmativeLabel || label == claudeFolderTrustExitLabel ||
		(len(label) >= claudeFolderTrustMinLabelStart && strings.HasPrefix(claudeTrustAffirmativeLabel, label))
}

// claudeFolderTrustLabelRead reads the affirmative label out of the option
// block: the unbroken run that holds the row the block began on, below the
// question and above the footer. whole is true when one of its rows is the
// label exactly; ok is false when the run does not hold the label at all, even
// joined across rows.
func claudeFolderTrustLabelRead(rows []claudeTrustRow, options, question, footer int) (whole, ok bool) {
	from, to := claudeTrustRunBounds(rows, options)
	from, to = max(from, question+1), min(to, footer-1)
	labels := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		if rows[i].label == claudeTrustAffirmativeLabel {
			return true, true
		}
		labels = append(labels, rows[i].label)
	}
	return false, strings.Contains(strings.Join(labels, " "), claudeTrustAffirmativeLabel)
}

// claudeFolderTrustFooterRead reads the footer: whole when it is one row, and
// ok when it is one row or the dialog's footer wrapped onto several.
func claudeFolderTrustFooterRead(rows []claudeTrustRow, footer int) (whole, ok bool) {
	end := footer
	for end+1 < len(rows) && !rows[end+1].blank {
		end++
	}
	if end == footer {
		return true, true
	}
	labels := make([]string, 0, end-footer+1)
	for i := footer; i <= end; i++ {
		labels = append(labels, rows[i].label)
	}
	return false, strings.Join(labels, " ") == claudeFolderTrustFooter
}
