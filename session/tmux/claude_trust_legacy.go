package tmux

import (
	"slices"
	"strings"
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
//	footer          optional "Enter to confirm…", only after the option block
//
// A region with the whole grammar and the cursor on Yes is answerable. A region
// that is a proper prefix of it — question and body with no options yet, or a
// Yes row whose No row is not painted — is partial, as is a complete one whose
// cursor is not on Yes. A region holding any row the dialog never paints is not
// the dialog: a ❯ row that is not an option (a composer draft), a frame rule
// between the question and the options (another box — the composer's), content
// below the option block other than its frame and footer. A question quoted
// inside prose is not a whole row, so it never opens a region at all.
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
func claudeLegacyTrustDialogOf(content string) claudeLegacyTrustVerdict {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
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
	var selected, yesSelected, hasNo bool
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
				phase = afterFooter
				continue
			case !claudeLegacyOptionRow(row.label):
				return claudeLegacyTrustNone
			}
		case afterOptions:
			switch {
			case row.blank || rule:
			case claudeTrustIsFooter(row.label):
				phase = afterFooter
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
		return claudeLegacyTrustAnswerable
	}
	return claudeLegacyTrustPartial
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
// both to blank, so this reads the raw line: box-drawing only, with at least one
// horizontal stroke.
func claudeTrustFrameRule(line string) bool {
	line = strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	if line == "" || strings.Trim(line, claudeTrustBoxDrawing) != "" {
		return false
	}
	return strings.ContainsAny(line, "─━═┄┈")
}
