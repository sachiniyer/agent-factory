package tmux

import (
	"fmt"
	"strings"
	"unicode"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/log"
)

// pasteChipMarker opens the collapsed-paste placeholder both Codex ("[Pasted
// Content 3207 chars]") and Claude ("[Pasted text #1 +10 lines]") draw instead
// of a long payload, in normalized (whitespace-free) form. A chip never names
// the text it holds, and real Codex draws the SAME chip for a pending paste and
// for a submitted message's transcript echo (daemon/configagent.go), so a chip
// on its own is never evidence about this delivery. It becomes evidence only
// through stagedEvidence's binding to our own Enter boundary.
const pasteChipMarker = "[Pasted"

// claudeComposerGlyph opens Claude Code's composer input row — the same glyph
// it draws for picker selection (claudeTrustSelectionGlyph) and for the
// transcript echo of a submitted prompt. Claude Code hides the terminal cursor
// in its ordinary composer (cursor_flag=0 — measured in claude_trust.go), so a
// cursor-anchored staged check can never fire for it; structure is the only
// row discriminator, exactly as it is for the trust dialogs.
const claudeComposerGlyph = "❯"

// stagedEvidence is the set of evidence kinds that are bound to THIS delivery:
// each newly appeared between the pre-paste baseline and the frame tmux
// captured in the same command queue as our Enter. Only bound kinds may
// classify a later frame as staged.
type stagedEvidence struct {
	tail bool
	chip bool
}

func (e stagedEvidence) any() bool { return e.tail || e.chip }

// boundEvidence reports which evidence kinds this paste introduced by the
// instant our Enter was sent. The baseline is the post-clear frame (or the
// pre-clear one when that capture failed), so a user draft the C-u could not
// clear, an older transcript echo, or a chip from a previous message is already
// counted there and cannot bind. The count must GROW, never merely be present.
func (p deliveryProbe) boundEvidence(boundaryText string) stagedEvidence {
	if !p.baselineCaptured || p.completion == "" {
		return stagedEvidence{}
	}
	return stagedEvidence{
		tail: strings.Count(boundaryText, p.completion) > p.completionBaseline,
		chip: strings.Count(boundaryText, pasteChipMarker) > strings.Count(p.baselineText, pasteChipMarker),
	}
}

// frameStill reports that nothing new was drawn between two normalized frames:
// the later one may only have lost rows off its top, the same test
// absenceStillProven applies before a redelivery (#4884). A composer that
// absorbed Enter as a literal newline passes — it grows by a blank row, which
// normalization erases, and at most scrolls the top away. A submit does not:
// the composer clears, the prompt echoes, a working indicator appears. Neither
// does user input: a typed character or a pasted chip is new text. An empty
// frame proves nothing.
func frameStill(before, after string) bool {
	return after != "" && strings.HasSuffix(before, after)
}

// capturePaneAndCursorState reads the cursor state and the pane grid in ONE
// tmux command list. tmux runs a client's command list to completion before
// servicing pane output again — the guarantee sendEnterAndCaptureBoundary
// already relies on — so the cursor row and the grid describe one instant: a
// queued Enter that dispatches mid-check cannot leave a stale pane paired with
// a fresh cursor. display-message runs first so its single output line is
// unambiguously the head of stdout. Any failure (including a tripped deadline)
// is "could not look" — callers treat that as no evidence, never a negative.
func (t *TmuxSession) capturePaneAndCursorState() (string, paneCursorState, bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	target := exactTarget(t.sanitizedName)
	out, err := t.outputTmuxBounded(ctx,
		"display-message", "-p", "-t", target, paneCursorStateFormat, ";",
		"capture-pane", "-p", "-t", target,
	)
	if err != nil {
		return "", paneCursorState{}, false
	}
	s := string(out)
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return "", paneCursorState{}, false
	}
	var cursor paneCursorState
	var visible int
	if _, err := fmt.Sscanf(strings.TrimSpace(s[:idx]), "%d %d %d",
		&cursor.Row, &cursor.Col, &visible); err != nil {
		return "", paneCursorState{}, false
	}
	cursor.Visible = visible != 0
	return s[idx+1:], cursor, true
}

// stagedInComposer reports whether bound evidence sits in the live composer of
// an atomic pane+cursor snapshot. It answers "is our payload still input, not
// transcript?" — the question the frame comparison cannot, since a pane whose
// application renders nothing on submit would also stand still.
//
// With a visible cursor, the cursor row is the anchor. The payload's own
// trailing newlines, and each Enter the composer absorbed as a newline
// (absorbedEnters), leave the cursor on blank continuation rows BELOW the text,
// so the check steps up over at most that many blank composer rows — never
// across a border or a non-blank row, which would leave the composer.
//
// With a hidden cursor (Claude's ordinary composer, cursor_flag=0) the anchor
// is structure: the evidence must end a row inside the LAST ❯-anchored block.
// A submitted prompt's echo cannot satisfy that, because the composer repaints
// a fresh ❯ row beneath it. The block runs across blank continuation rows (a
// multi-paragraph draft) and ends at a border or at the frame's end.
func stagedInComposer(pane string, cursor paneCursorState, probe deliveryProbe, bound stagedEvidence, absorbedEnters int) bool {
	rows := strings.Split(strings.TrimSuffix(pane, "\n"), "\n")
	if !cursor.Visible {
		last := -1
		for i, r := range rows {
			if strings.HasPrefix(normalizeDelivery(r), claudeComposerGlyph) {
				last = i
			}
		}
		if last < 0 {
			return false
		}
		for j := last; j < len(rows); j++ {
			norm := normalizeDelivery(rows[j])
			if norm == "" {
				if blankComposerRow(rows[j]) {
					continue
				}
				return false
			}
			if composerRowHolds(norm, probe, bound) {
				return true
			}
		}
		return false
	}

	if cursor.Row < 0 || cursor.Row >= len(rows) {
		return false
	}
	row := cursor.Row
	for steps := probe.trailingNewlines + absorbedEnters; steps > 0 && row > 0 && blankComposerRow(rows[row]); steps-- {
		row--
	}
	return composerRowHolds(normalizeDelivery(rows[row]), probe, bound)
}

// composerRowHolds matches bound evidence on ONE normalized composer row: the
// payload's completion tail ending the row (or, for a pane narrow enough to
// wrap the draft, a row that IS the tail's end), or a paste chip on the row.
// The row may carry a leading prompt glyph (>, ❯, ›) that is not part of the
// payload; both suffix directions tolerate it.
func composerRowHolds(norm string, probe deliveryProbe, bound stagedEvidence) bool {
	if norm == "" {
		return false
	}
	if bound.tail {
		trimmed := strings.TrimLeftFunc(norm, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		if strings.HasSuffix(norm, probe.completion) ||
			(len([]rune(trimmed)) >= minDistinctiveFragment && strings.HasSuffix(probe.completion, trimmed)) {
			return true
		}
	}
	return bound.chip && strings.Contains(norm, pasteChipMarker)
}

// blankComposerRow is a row holding nothing but whitespace and the vertical
// edges of a composer box — an empty line INSIDE the input, as opposed to a
// horizontal border, which also normalizes to "" but marks the composer's edge.
func blankComposerRow(raw string) bool {
	return strings.TrimFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || r == '│' || r == '┃' || r == '|'
	}) == ""
}

// trailingNewlines counts the row breaks in a payload's trailing whitespace:
// the blank rows a composer that renders pastes literally draws below the text.
func trailingNewlines(s string) int {
	trimmed := strings.TrimRightFunc(s, unicode.IsSpace)
	return strings.Count(s[len(trimmed):], "\n")
}

// remedyStrandedSubmit is the #4200 repair: a composer still rendering a paste
// can absorb Enter as a literal newline, leaving the whole prompt staged and
// unsubmitted while the pre-Enter observation already reads landed. After a
// short grace it looks once more and, when the draft is provably still staged,
// sends ONE more Enter — never a re-paste.
//
// The property that authorizes that Enter is that it can only submit what our
// first Enter was already sent to submit. Three conditions establish it, and
// each is necessary:
//
//  1. Binding. The evidence — this payload's completion tail, or a paste chip —
//     newly appeared between the pre-paste baseline and the frame captured in
//     the same tmux command queue as our Enter (boundEvidence). Evidence that
//     predates the paste cannot bind, whoever drew it.
//  2. Stillness. The grace frame has drawn nothing new since that boundary
//     (frameStill). The composer therefore holds exactly what it held when our
//     Enter was sent: no submit happened (it would have cleared the composer
//     and drawn an echo or indicator), and no user typed or pasted into it (new
//     text). This is what makes a chip usable at all: the agent draws the same
//     chip for a user's paste and for a submitted echo, but neither can appear
//     in a frame that has not changed.
//  3. Position. The bound evidence sits in the live composer (stagedInComposer),
//     so the still frame is a staged draft, not a transcript that happens not to
//     have moved.
//
// What the conditions do not cover: the gap between the grace capture and the
// remedy keystroke is check-then-act, like absenceStillProven's. The remedy's
// own boundary frame is compared against the grace frame, so anything drawn in
// that gap downgrades the report instead of being called delivered — it can be
// detected, not prevented. The raw attach stream stays lockless as everywhere
// on this path; the daemon's attach defer (#1586) is the guard for a user typing
// in a pane.
//
// The verdict never rounds up. Bound evidence that stays staged, a pane that
// changed while staged, a remedy whose outcome cannot be re-read, and a remedy
// Enter sent into a changed frame all report sent-unverified. Only a draft that
// left the composer after the remedy keeps the observation it had before Enter —
// landed stays delivered, the same standard the ordinary path applies to a
// first Enter.
func (t *TmuxSession) remedyStrandedSubmit(probe deliveryProbe, observation deliveryObservation, boundary string, boundaryOK bool) deliveryObservation {
	if !boundaryOK {
		return observation
	}
	boundaryText := normalizeDelivery(xansi.Strip(boundary))
	bound := probe.boundEvidence(boundaryText)
	if !bound.any() {
		return observation
	}

	pasteDeliverySleep(strandedSubmitGrace)
	pane, cursor, ok := t.capturePaneAndCursorState()
	if !ok || !stagedInComposer(pane, cursor, probe, bound, 1) {
		return observation
	}
	graceText := normalizeDelivery(pane)
	unverified := deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	if !frameStill(boundaryText, graceText) {
		log.WarningLog.Printf("submit: session %q shows this prompt staged in the composer after Enter, but the pane changed since the Enter, "+
			"so the composer may no longer hold only this prompt; withholding the remedy Enter and reporting sent-unverified (#4200). Pane tail: %s",
			t.sanitizedName, oneLineTail(pane))
		return unverified
	}

	log.WarningLog.Printf("submit: session %q still holds this prompt staged in the composer and the pane has not changed since Enter; "+
		"the keystroke was absorbed, so sending one more Enter to submit the same draft (#4200)", t.sanitizedName)
	remedyBoundary, remedyBoundaryOK, err := t.sendEnterAndCaptureBoundary()
	if err != nil {
		log.WarningLog.Printf("submit: remedy Enter for session %q did not reach tmux; reporting sent-unverified: %v", t.sanitizedName, err)
		return unverified
	}
	if !remedyBoundaryOK {
		t.deferDeliveryBaseline()
		return unverified
	}
	// The remedy's boundary is the new delivery boundary: churn after it is the
	// agent responding to the submitted draft.
	t.seedDeliveryBaseline(remedyBoundary)
	if !frameStill(graceText, normalizeDelivery(xansi.Strip(remedyBoundary))) {
		log.ErrorLog.Printf("submit: the pane of session %q changed between the staged-draft check and the remedy Enter, "+
			"so that Enter may have submitted more than this prompt; reporting sent-unverified (#4200). Pane tail: %s",
			t.sanitizedName, oneLineTail(remedyBoundary))
		return unverified
	}

	// Submitted means the draft LEFT: the pane changed after the remedy Enter
	// and the bound evidence is no longer in the composer. Either alone is not
	// enough — a composer that absorbed the remedy Enter too stands still with
	// the draft two blank rows above the cursor, and a changed pane may be the
	// draft still sitting under new text.
	pasteDeliverySleep(strandedSubmitSettle)
	settled, settledCursor, ok := t.capturePaneAndCursorState()
	if !ok || frameStill(graceText, normalizeDelivery(settled)) ||
		stagedInComposer(settled, settledCursor, probe, bound, 2) {
		return unverified
	}
	if observation.outcome == deliveryObservedLanded {
		return deliveryObservation{outcome: deliveryObservedLanded, pane: settled}
	}
	return deliveryObservation{outcome: deliveryObservedUnverified, pane: settled}
}
