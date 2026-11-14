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

// errClaudeFolderTrustQuestionOffScreen is why af refuses a folder-trust
// dialog whose options and footer it can see but whose question it cannot. It
// says what af saw, not why (#7284 review, R4): a pane too short scrolls the
// question off, but a quote with no composer painted below it yet reads the
// same way.
var errClaudeFolderTrustQuestionOffScreen = errors.New(
	"its options and footer are on screen but its question is not, so af cannot tell it from a quote of it (if the pane is too short for the dialog, make it taller; or answer the dialog in the session)")

// claudeFolderTrustUnreadableWhy says what af saw of a dialog
// claudeFolderTrustDialogOf reads as unreadable.
func claudeFolderTrustUnreadableWhy(screen visibleText) error {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
	}
	if _, _, ok := claudeFolderTrustQuestionRows(rows); !ok {
		return errClaudeFolderTrustQuestionOffScreen
	}
	return errClaudeFolderTrustUnreadable
}

// claudeFolderTrustVerdict is what one capture says about the dialog.
type claudeFolderTrustVerdict int

const (
	// claudeFolderTrustNone: the pane is not showing the dialog.
	claudeFolderTrustNone claudeFolderTrustVerdict = iota
	// claudeFolderTrustPartial: a proper prefix of the dialog; Claude Code is
	// still painting it. Hold: send nothing, and do not report the pane clear.
	claudeFolderTrustPartial
	// claudeFolderTrustUnreadable: the whole dialog, with a row af reads by
	// wrapped onto two, or with its question scrolled off the top of a pane
	// too short for it. Hold without a key, and say why once.
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
//	closing frame optional, a frame rule directly above the footer
//	footer        "Enter to confirm…", then only blank rows and frame chrome
//
// A ❯ row above the question is no part of that: a launch modal owns the pane
// from the top. But it is no composer BELOW the dialog either, so it does not
// show a working agent; the region holds, and is never answered (#7280).
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
// wraps well before the options or the footer do. A pane too short for the
// dialog scrolls its question off the top instead; the dialog's own option
// block and footer are still the last content, which only the live dialog
// paints (claudeFolderTrustHeadless, #7280).
//
// A region that breaks that grammar is no dialog only when the working agent's
// composer is painted below the dialog's option rows (claudeFolderTrustBroken).
// A working agent always paints its composer below its output, so a dialog it
// quotes — a diff or a grep of this file, a review — reads as no dialog, and
// the create delivers its prompt to the agent. #3587 held such a pane instead,
// refusing the quote until the create failed. Any other row the dialog does not
// paint is not evidence of a working agent, because the live dialog can show
// one: Claude Code's footer element reads "Press Ctrl-C again to exit" for a
// moment after one Ctrl-C (#7275 review). So such a region is partial, and
// holds without a key.
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
	if !ok {
		if claudeFolderTrustHeadless(lines, rows) {
			return claudeFolderTrustUnreadable
		}
		return claudeFolderTrustNone
	}
	verdict := claudeFolderTrustRegionOf(lines, rows, first, last, lastPainted)
	if firstCursor >= 0 && firstCursor < first && verdict != claudeFolderTrustNone {
		// A ❯ above the question: no composer below, so hold, never answer.
		return claudeFolderTrustPartial
	}
	return verdict
}

// claudeFolderTrustRegionOf reads the region the question opens, from rows
// first to last, through the grammar claudeFolderTrustDialogOf documents.
func claudeFolderTrustRegionOf(lines []string, rows []claudeTrustRow, first, last, lastPainted int) claudeFolderTrustVerdict {
	wrapped := first != last
	footerRow := func(i int) bool {
		// A ❯ row is never the footer: it is a draft that reads like one.
		return i < len(rows) && !rows[i].selected && claudeTrustIsFooter(rows[i].label)
	}

	const (
		inBody = iota
		inOptions
		afterOptions
		inFooter
		afterFooter
	)
	phase, options, footer := inBody, -1, -1
	broken := func(at int) claudeFolderTrustVerdict {
		return claudeFolderTrustBroken(lines, rows, at, lastPainted)
	}
	for i := last + 1; i < len(rows); i++ {
		row, rule := rows[i], claudeTrustFrameRule(lines[i])
		switch phase {
		case inBody:
			switch {
			case rule || claudeTrustIsFooter(row.label):
				// A row that reads as the footer, ❯ or not, before any option
				// breaks the grammar (#7284 review, R1).
				return broken(i)
			case row.blank:
			case claudeFolderTrustOptionStarts(rows, i, i == lastPainted):
				phase, options = inOptions, i
			case row.selected:
				// A ❯ row that is none of this dialog's options: a composer.
				return broken(i)
			}
		case inOptions, afterOptions:
			switch {
			case rule && footerRow(i+1):
				// The dialog's closing frame, directly above its footer.
				phase = afterOptions
			case rule:
				return broken(i)
			case row.blank:
				phase = afterOptions
			case footerRow(i):
				phase, footer = inFooter, i
			case claudeTrustIsFooter(row.label):
				// A ❯ row that reads as the footer is neither the footer nor
				// an option row: it breaks the grammar, as on master, rather
				// than taking the affirmative's run for an option af would
				// steer away from (#7284 review, R1).
				return broken(i)
			case phase == inOptions:
				// Another option row.
			case i == lastPainted && strings.HasPrefix(claudeTrustAffordancePrefix, row.label):
				// The footer, cut short mid-paint.
				return claudeFolderTrustPartial
			default:
				return broken(i)
			}
		case inFooter:
			switch {
			case row.blank:
				phase = afterFooter
			case row.selected:
				return broken(i)
			}
			// Other content straight below the footer row is the rest of a
			// footer the pane wrapped, if claudeFolderTrustFooterRead reads it so.
		case afterFooter:
			if !row.blank && !rule {
				return broken(i)
			}
		}
	}
	if phase < inFooter {
		return claudeFolderTrustPartial
	}

	label, ok := claudeFolderTrustLabelRead(rows, options, last, footer)
	if !ok {
		if claudeFolderTrustOwnOptions(rows, options, last, footer) {
			// The dialog's own option rows without its full affirmative row:
			// a repaint after af's Down caught half done, the affirmative row
			// erased or its ❯ row half written (#7280).
			return claudeFolderTrustPartial
		}
		// A footer under options, but not this dialog's options.
		return claudeFolderTrustNone
	}
	whole, ok := claudeFolderTrustFooterRead(rows, footer)
	if !ok {
		// The rows straight below the footer are not a wrap of it.
		return broken(footer + 1)
	}
	if wrapped || !label || !whole {
		return claudeFolderTrustUnreadable
	}
	return claudeFolderTrustComplete
}

// claudeFolderTrustBroken decides a region that breaks the dialog's grammar at
// row from (#7275 review, R1). It is no dialog only when a row from there down
// is the working agent's composer and no row the dialog paints — an option row
// or its footer — is painted below it (#7284 review, R2): the composer of a
// working agent is the last thing in its pane, so it is what shows that the
// dialog's text above is output being quoted. Any
// other row the grammar does not know — a swapped footer, a notice, a tip, a
// stray line — or a frame rule on its own, which is the dialog's own chrome
// too, is no evidence that an agent is working. The region holds as partial.
func claudeFolderTrustBroken(lines []string, rows []claudeTrustRow, from, lastPainted int) claudeFolderTrustVerdict {
	composer, option := -1, -1
	for i := from; i < len(rows); i++ {
		switch {
		case claudeFolderTrustComposerRow(lines, rows, i, i == lastPainted):
			composer = i
		case rows[i].selected || claudeFolderTrustOptionLabel(rows[i].label) || claudeTrustIsFooter(rows[i].label):
			option = i
		}
	}
	if composer >= 0 && composer > option {
		return claudeFolderTrustNone
	}
	return claudeFolderTrustPartial
}

// claudeFolderTrustComposerRow reports whether row i is the working agent's
// prompt row rather than anything the dialog paints:
//
//   - a ❯ row that is none of the dialog's options. The dialog paints one ❯,
//     on an option; Claude Code leads its composer prompt with the same glyph,
//     between two rules (testdata/claude_2_1_322_composer_after_mcp_decline.ansi),
//     and a permission prompt in the composer's place carries one on its own
//     picker.
//   - a "> " row straight below a frame rule: the boxed composer older builds
//     draw ("╭───╮" over "│ > … │").
//
// A ❯ row holding one of the dialog's own labels, or cut short of one, in a run
// of nothing but such rows is never one. A ❯ row between the composer's two
// plain rules always is. A frame rule alone is not one: the dialog's closing
// frame is a rule too.
func claudeFolderTrustComposerRow(lines []string, rows []claudeTrustRow, i int, lastPainted bool) bool {
	if rows[i].selected {
		switch {
		case claudeFolderTrustOwnRun(rows, i):
			// One of the dialog's own labels, or a row cut short of one, in a
			// run of nothing else: the dialog's, whatever is around it
			// (#7280). The empty ❯ is not one.
			return false
		case i > 0 && i+1 < len(lines) && claudeComposerRule(lines[i-1]) && claudeComposerRule(lines[i+1]):
			// Between the composer's two rules, the way it frames its prompt,
			// even as the last row painted (#7280).
			return true
		}
		return !claudeFolderTrustOptionStarts(rows, i, lastPainted)
	}
	label := rows[i].label
	return i > 0 && claudeTrustFrameRule(lines[i-1]) && (label == ">" || strings.HasPrefix(label, "> "))
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

// claudeComposerRule reports whether line is a rule of the kind Claude Code
// draws above and below its composer: horizontal strokes and nothing else
// (testdata/claude_2_1_322_composer_after_mcp_decline.ansi). A box's corners
// and tees ("╭──╮", "├──┤", "╰──╯") are a framed dialog's edges, not the
// composer's (#7284 review, R5).
func claudeComposerRule(line string) bool {
	line = strings.TrimSpace(line)
	return line != "" && strings.Trim(line, "─━═┄┈") == ""
}

// claudeFolderTrustOwnRun reports whether every row of the unbroken run holding
// row i carries one of the dialog's own option labels or a non-empty prefix of
// one: the shape a repaint caught mid-row leaves of the dialog's options, such
// as "❯ No, ex" (#7280). A picker of the agent's own is not one, even when its
// ❯ row reads as a prefix: a permission prompt's "❯ 1. Yes" shares its run with
// rows the dialog never paints ("2. Yes, and don't ask again …").
func claudeFolderTrustOwnRun(rows []claudeTrustRow, i int) bool {
	from, to := claudeTrustRunBounds(rows, i)
	for j := from; j <= to; j++ {
		label := rows[j].label
		if label == "" ||
			!(strings.HasPrefix(claudeTrustAffirmativeLabel, label) || strings.HasPrefix(claudeFolderTrustExitLabel, label)) {
			return false
		}
	}
	return true
}

// claudeFolderTrustOwnOptions reports whether the option block, read the way
// claudeFolderTrustLabelRead reads it, holds one of the dialog's own rows: its
// exit row, or the affirmative row whole or cut to at least
// claudeFolderTrustMinLabelStart bytes.
func claudeFolderTrustOwnOptions(rows []claudeTrustRow, options, question, footer int) bool {
	from, to := claudeTrustRunBounds(rows, options)
	from, to = max(from, question+1), min(to, footer-1)
	for i := from; i <= to; i++ {
		if claudeFolderTrustOptionLabel(rows[i].label) {
			return true
		}
	}
	return false
}

// claudeFolderTrustHeadless reports a dialog whose question has scrolled off
// the top of a pane too short for it (#7280). With no question on screen there
// is no region to read, so it is anchored where claudeMCPServerUnreadableOf is,
// on what only the live dialog paints at the bottom of the pane:
//
//   - the last painted line, a frame rule counted as painted, is the footer
//     (or the last row of the dialog's footer wrapped), and no ❯ row;
//   - above it, after blank rows and frame chrome, an unbroken run of option
//     rows holding the affirmative row, whole or as the first row a narrow
//     pane leaves of it;
//   - from that run to the top of the pane, nothing the dialog does not paint
//     there: no ❯ row, no frame rule, no footer. The dialog's top rule scrolled
//     off with its question.
//
// A quote can never meet it: a working agent paints its composer below.
func claudeFolderTrustHeadless(lines []string, rows []claudeTrustRow) bool {
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 || rows[end].blank {
		return false
	}
	footer := end
	for footer > 0 && !rows[footer-1].blank {
		footer--
	}
	for i := footer; i <= end; i++ {
		if rows[i].selected {
			return false
		}
	}
	if !claudeTrustIsFooter(rows[footer].label) {
		return false
	}
	if _, ok := claudeFolderTrustFooterRead(rows, footer); !ok {
		return false
	}
	// Blank rows and frame chrome between the options and the footer: the
	// closing frame of a boxed dialog, or a rule this reading cannot place. A
	// rule here only ever makes a pane hold.
	i := footer - 1
	for i >= 0 && rows[i].blank {
		i--
	}
	if i < 0 {
		return false
	}
	from, _ := claudeTrustRunBounds(rows, i)
	affirmative := false
	for j := from; j <= i; j++ {
		label := rows[j].label
		affirmative = affirmative || label == claudeTrustAffirmativeLabel ||
			(len(label) >= claudeFolderTrustMinLabelStart && strings.HasPrefix(claudeTrustAffirmativeLabel, label))
	}
	if !affirmative {
		return false
	}
	for j := from - 1; j >= 0; j-- {
		if rows[j].selected || claudeTrustFrameRule(lines[j]) || claudeTrustIsFooter(rows[j].label) {
			return false
		}
	}
	return true
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
