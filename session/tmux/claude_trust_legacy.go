package tmux

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// claudeLegacyTrustQuestion is the legacy folder-trust dialog's question, the
// wording older Claude Code builds used before the "Quick safety check" dialog.
const claudeLegacyTrustQuestion = "Do you trust the files in this folder?"

// claudeLegacyTrustVerdict is what a capture says about the legacy dialog.
type claudeLegacyTrustVerdict int

const (
	// claudeLegacyTrustNone: the pane is not showing the legacy dialog.
	claudeLegacyTrustNone claudeLegacyTrustVerdict = iota
	// claudeLegacyTrustPartial: the pane shows the dialog, but not in a state
	// af may answer — still painting, or the cursor is not on Yes. Hold: send
	// nothing, and do not report the pane clear.
	claudeLegacyTrustPartial
	// claudeLegacyTrustAnswerable: the whole dialog, cursor on Yes. Enter
	// accepts it.
	claudeLegacyTrustAnswerable
)

// claudeLegacyTrustDialogOf decides whether the legacy folder-trust dialog owns
// the pane. It runs on the daemon's continuous poll against arbitrary agent
// output, and Enter typed into a working composer cannot be taken back, so it
// decides from ONE property rather than from cases:
//
// The dialog owns the pane only when its question, painted as a whole row of
// its own, opens the pane's trailing region, and every row from the question to
// the bottom of the pane is one the dialog itself paints, in the dialog's order:
//
//	question        exactly claudeLegacyTrustQuestion
//	body rows       plain text (path, explanation, URL): no ❯, no frame rule
//	option block    one unbroken run of option rows — a Yes and a No option,
//	                inline ("❯ Yes  No") or stacked ("❯ 1. Yes" / "2. No")
//	closing frame   optional, only after the option block
//	footer          "Enter to confirm…", only after the option block
//
// A region with the whole grammar, its whole footer last and the cursor on Yes
// is answerable. A region that is a proper prefix of it — question and body with
// no options yet, a Yes row whose No row is not painted, or the whole option
// block with its footer not yet painted or only begun — is partial, as is a
// complete one whose cursor is not on Yes. Every build that paints this dialog
// paints its footer last (claudeTrustFooters, #7584), so a dialog without
// its whole footer is one still being painted. A region holding any row the
// dialog never paints is not the dialog: a ❯ row that is not an option (a
// composer draft), a frame rule between the question and the options (another
// box — the composer's), content below the option block other than its frame
// and footer. A question quoted inside prose is not a whole row, so it never
// opens a region at all.
//
// The inline "❯ Yes  No" option row is the one shape a working composer's draft
// can reproduce verbatim: the composer's prompt glyph is the same ❯ the picker
// uses as its selection cursor (claudeTrustSelectionGlyph), and a user draft
// "Yes No" reduces to the same inline-pair label. The frame rule is the only
// thing that rejects a FRAMED composer, so an equivalent no-frame composer whose
// question sits in the transcript above its draft would reach the option branch
// unchanged and be classified as answerable — the Enter af then taps submits the
// user's draft. A launch modal, by contrast, owns a fresh pane from the top, so
// its question is the first non-blank content; a composer always sits below agent
// output. The inline-pair branch therefore also requires no prose above the
// question row (frame chrome is reduced to blank by claudeTrustRowOf, so a boxed
// modal still satisfies it). The stacked picker needs no such guard: a single
// composer line cannot paint two option rows.
//
// The hidden-cursor oracle the codex branch uses is not available here (Claude
// Code hides the cursor in its composer too; see claudeTrustAffordancePrefix),
// so the grammar carries the whole weight.
func claudeLegacyTrustDialogOf(screen visibleText) claudeLegacyTrustVerdict {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	question := -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if rows[i].label == claudeLegacyTrustQuestion {
			question = i
		}
	}
	if question < 0 {
		return claudeLegacyTrustNone
	}

	const (
		inBody = iota
		inOptions
		afterOptions
		afterFooter
	)
	phase := inBody
	var selected, yesSelected, hasNo, footerWhole bool
	var optionRows int
	var inlinePairBlock bool
	for i := question + 1; i < len(rows); i++ {
		row := rows[i]
		rule := claudeTrustFrameRule(lines[i])
		switch phase {
		case inBody:
			switch {
			case rule:
				return claudeLegacyTrustNone
			case row.blank:
			case claudeTrustIsFooter(row.label):
				return claudeLegacyTrustNone
			case claudeLegacyOptionRow(row.label):
				phase = inOptions
			case row.selected:
				return claudeLegacyTrustNone
			}
		case inOptions:
			switch {
			case row.blank || rule:
				phase = afterOptions
			case claudeTrustIsFooter(row.label):
				phase, footerWhole = afterFooter, claudeTrustFooterWhole(row.label)
				continue
			case !claudeLegacyOptionRow(row.label):
				return claudeLegacyTrustNone
			}
		case afterOptions:
			switch {
			case row.blank || rule:
			case claudeTrustIsFooter(row.label):
				phase, footerWhole = afterFooter, claudeTrustFooterWhole(row.label)
			default:
				return claudeLegacyTrustNone
			}
			continue
		case afterFooter:
			if !row.blank {
				return claudeLegacyTrustNone
			}
			continue
		}
		if phase != inOptions {
			continue
		}
		// An option row.
		optionRows++
		yes, no := claudeLegacyOptionKinds(row.label)
		inlinePairBlock = optionRows == 1 && yes && no
		if row.selected {
			if selected {
				return claudeLegacyTrustNone
			}
			selected, yesSelected = true, yes
		}
		hasNo = hasNo || no
	}

	// phase == inBody (question and body painted, no option yet) falls through
	// to partial with selected false.
	if phase != inBody && selected && yesSelected && hasNo {
		// The inline "❯ Yes  No" row is indistinguishable from a working
		// composer's draft on a single visible capture — same glyph, same
		// label — and a no-frame composer reaches this branch unchanged. A
		// launch modal owns a fresh pane from the top, so its question is the
		// first non-blank content; a composer always sits below agent output.
		// Require that for the inline pair; the stacked picker needs no guard
		// (a single composer line cannot paint two option rows).
		if inlinePairBlock && claudeLegacyProseAboveQuestion(rows, question) {
			return claudeLegacyTrustNone
		}
		// The footer is painted last, below the options. Until it is whole
		// the dialog is still being painted: hold, and answer it on the
		// frame that shows it whole (#7584).
		if !footerWhole {
			return claudeLegacyTrustPartial
		}
		// A footer inside a box that closes under it is text in a boxed
		// composer's draft whose prompt row has scrolled off, not a live
		// modal: no build paints its footer inside a box (the one that
		// boxes the dialog paints the footer under the box, and the pinned
		// live fixture legacyTrustBoxed does), so such a frame is a quoted
		// draft, and the Enter af taps would submit it. claudeTrustRowOf
		// strips box sides and reduces a closing edge to blank, so the
		// grammar above reads a boxed quote as the dialog's own rows; hold
		// it, and send nothing. claudeLaunchDialogFooterInClosedBox models
		// this same shape for the remnant reader (claude_launch_dialog_remnant.go);
		// reusing it keeps the answering path and the remnant reader
		// consistent. claudeFolderTrustReadToBottom also reads a Partial as
		// a hold, so the remnant reader's own closed-box guard stays
		// reachable for the same shape it already keeps.
		//
		// The same hold does not even need the box's outline whole: a
		// footer row that itself starts with a box-side glyph is painted
		// inside the box, whatever closes the box below it — a bottom
		// corner edge, a bare ━━━…━ rule a bottom corner never opened, or
		// nothing at all — and however much of the right border a resize
		// before the repaint clipped away. No build paints its footer row
		// with a box side either, so the side alone proves the quote
		// (#7785).
		pane := claudeLaunchDialogPaneOf(screen)
		footerRow := len(pane.index) - 1
		if claudeTrustLeftBordered(pane.lines[pane.index[footerRow]]) ||
			claudeLaunchDialogFooterInClosedBox(pane, footerRow) {
			return claudeLegacyTrustPartial
		}
		return claudeLegacyTrustAnswerable
	}
	return claudeLegacyTrustPartial
}

// claudeTrustFooters are the footers Claude Code paints under its folder-trust
// dialog, the legacy wording and the reworded one alike. Every build af
// supports paints one of them as the dialog's last row (#7584, #7618):
//   - 2.0.25 through 2.1.19 draw every arm of the dialog with one component,
//     the legacy and the reworded wording included, and paint "Esc to exit"
//     through 2.0.62 and "Esc to cancel" from 2.0.63;
//   - from 2.1.20 the dialog is the reworded one, and its footer is "Esc to
//     cancel".
//
// The one other text that row takes, "Press Ctrl-C again to exit" while an
// exit key press is pending, is a row neither grammar reads as the dialog's.
var claudeTrustFooters = []string{claudeLegacyTrustFooter, claudeFolderTrustFooter}

// claudeTrustFooterWhole reports whether a footer row is a whole
// claudeTrustFooters entry rather than one still being painted.
func claudeTrustFooterWhole(label string) bool {
	return slices.Contains(claudeTrustFooters, strings.Join(strings.Fields(label), " "))
}

// claudeLegacyProseAboveQuestion reports whether any non-blank row is painted
// above the question row. claudeTrustRowOf reduces a frame-only row (box chrome)
// to blank, so a boxed modal — whose only content above the question is its top
// border — still passes; a transcript that quotes the question beneath other
// agent output does not.
func claudeLegacyProseAboveQuestion(rows []claudeTrustRow, question int) bool {
	for i := 0; i < question; i++ {
		if !rows[i].blank {
			return true
		}
	}
	return false
}

// claudeLegacyYesLabels and claudeLegacyNoLabels are the option labels the
// legacy picker renders, lower-cased, after claudeTrustRowOf has stripped the
// ❯ glyph and the leading ordinal. They are matched exactly, as
// claudeMCPOptionLabels are: a prefix test would take a composer draft
// ("Yes, I will fix that", "Yesterday…") for an option.
var (
	claudeLegacyYesLabels = []string{"yes", "yes, proceed"}
	claudeLegacyNoLabels  = []string{"no", "no, exit"}
)

// claudeLegacyOptionKinds reports whether an option label carries the Yes
// option, the No option, or both (the inline "Yes  No" row).
func claudeLegacyOptionKinds(label string) (yes, no bool) {
	lower := strings.ToLower(strings.TrimSpace(label))
	if claudeLegacyInlinePair(strings.Fields(lower)) {
		return true, true
	}
	return slices.Contains(claudeLegacyYesLabels, lower), slices.Contains(claudeLegacyNoLabels, lower)
}

// claudeLegacyOptionRow reports whether label is a row of the legacy option
// block.
func claudeLegacyOptionRow(label string) bool {
	yes, no := claudeLegacyOptionKinds(label)
	return yes || no
}

// claudeLegacyInlinePair matches the single-row picker "Yes  No" or
// "Yes  2. No" (the No option keeps its own ordinal; only the leading one is
// stripped).
func claudeLegacyInlinePair(fields []string) bool {
	switch len(fields) {
	case 2:
		return fields[0] == "yes" && fields[1] == "no"
	case 3:
		return fields[0] == "yes" && claudeLegacyPickerOrdinal(fields[1]) && fields[2] == "no"
	}
	return false
}

// claudeLegacyPickerOrdinal reports whether s is an "N." ordinal token.
func claudeLegacyPickerOrdinal(s string) bool {
	if len(s) < 2 || s[len(s)-1] != '.' {
		return false
	}
	for _, r := range s[:len(s)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// claudeTrustIsFooter reports whether label is the modal's footer.
func claudeTrustIsFooter(label string) bool {
	return strings.HasPrefix(label, claudeTrustAffordancePrefix)
}

// claudeTrustFrameRule reports whether line is a horizontal frame edge — a box's
// top or bottom border, or the rules Claude Code draws around its composer —
// rather than a blank row or a side-bordered row. claudeTrustRowOf reduces
// both to blank, so this reads the line itself: box-drawing only, with at least
// one horizontal stroke. The Claude parsers hand it rows of a visibleText;
// Gemini's hands it raw capture rows, so it strips SGR from those.
func claudeTrustFrameRule(line string) bool {
	if strings.IndexByte(line, '\x1b') >= 0 {
		line = ansiCSISequence.ReplaceAllString(line, "")
	}
	line = strings.TrimSpace(line)
	if line == "" || strings.Trim(line, claudeTrustBoxDrawing) != "" {
		return false
	}
	return strings.ContainsAny(line, "─━═┄┈")
}

// claudeTrustFrameClosingRule reports whether line is a framed box's closing
// (bottom) edge: a frame rule whose left corner is a bottom corner (╰, └, ╚,
// ┗), the way Claude Code paints below a boxed composer (╰…╯). A framed
// dialog's top border (╭…╮) is not. Used by the case-3 scan to reject a
// top-cropped boxed composer by its closing edge alone — the opening rule has
// scrolled off the top, so the prompt above it is not welded on its other
// side (#7399 review).
func claudeTrustFrameClosingRule(line string) bool {
	if strings.IndexByte(line, '\x1b') >= 0 {
		line = ansiCSISequence.ReplaceAllString(line, "")
	}
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "╰") || strings.HasPrefix(line, "└") ||
		strings.HasPrefix(line, "╚") || strings.HasPrefix(line, "┗")
}

// claudeTrustLeftBordered reports whether line begins with a box's side
// border: the left edge of a row painted inside a box. Where
// claudeTrustBordered wants both sides on the row, a quoted row keeps this
// one even when a resize or an unfinished repaint took the right border, and
// even when no bottom edge ever closes the box (#7785).
func claudeTrustLeftBordered(line string) bool {
	line = strings.TrimSpace(line)
	first, _ := utf8.DecodeRuneInString(line)
	return strings.ContainsRune(claudeTrustBoxSides, first)
}
