package tmux

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/log"
)

// composerStaged classifies post-Enter evidence that this delivery's payload is
// still sitting in the composer, unsubmitted. The distinction matters for the
// verdict: a tail sighting is prompt-specific (the payload itself was observed),
// while a collapsed-paste placeholder proves only that SOME paste is staged —
// the agents' chips never name which text they hold.
type composerStaged int

const (
	composerStagedNone composerStaged = iota
	// composerStagedChip: a paste chip such as "[Pasted text #1 +10 lines]" or
	// "[Pasted Content 3207 chars]" sits on the cursor row.
	composerStagedChip
	// composerStagedTail: this payload's completion tail ends at the live cursor.
	composerStagedTail
)

// claudeComposerGlyph opens Claude Code's composer input row — the same glyph
// it draws for picker selection (claudeTrustSelectionGlyph) and for the
// transcript echo of a submitted prompt. Claude Code hides the terminal cursor
// in its ordinary composer (cursor_flag=0 — measured in claude_trust.go), so a
// cursor-anchored staged check can never fire for it; structure is the only
// discriminator, exactly as it is for the trust dialogs.
const claudeComposerGlyph = "❯"

// capturePaneAndCursorState reads the cursor state and the pane grid in ONE
// tmux command list. tmux runs a client's command list to completion before
// servicing pane output again — the guarantee sendEnterAndCaptureBoundary
// already relies on — so the cursor row and the grid describe one instant: a
// queued Enter that dispatches mid-check can no longer leave a stale pane
// paired with a fresh cursor and misread a submitted draft as staged.
// display-message runs first so its single output line is unambiguously the
// head of stdout. Any failure (including a tripped deadline) is "could not
// look" — the callers treat that as no evidence, never a negative.
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

// stagedDraftInSnapshot reports whether this delivery's prompt is still visibly
// staged in the composer in a pane+cursor snapshot taken atomically. With a
// live cursor, the cursor row is the one position where visible text is
// provably unsubmitted input: a dispatched prompt's echo may keep the same tail
// visible in the transcript, but the cursor row itself then holds the fresh
// empty composer. With a hidden cursor (Claude's ordinary composer sets
// cursor_flag=0) the row anchor is unavailable, so the check falls back to
// structure: the payload's tail must end a row inside the LAST composer-glyph
// block, which a submitted prompt's echo cannot satisfy because a fresh
// composer row repaints beneath it.
func stagedDraftInSnapshot(pane string, cursor paneCursorState, probe deliveryProbe) composerStaged {
	normalized := normalizeDelivery(pane)
	tailVisible := probe.completion != "" && strings.Contains(normalized, probe.completion)
	chipVisible := strings.Contains(normalized, "[Pasted")
	if !tailVisible && !chipVisible {
		return composerStagedNone
	}
	if !cursor.Visible {
		return stagedDraftHiddenCursor(pane, probe, tailVisible, chipVisible)
	}
	row, ok := paneRowAt(pane, cursor.Row)
	if !ok {
		return composerStagedNone
	}
	return classifyComposerRow(normalizeDelivery(row), probe, tailVisible, chipVisible)
}

// classifyComposerRow matches staged-draft evidence on ONE normalized composer
// row: the payload's completion tail ending the row (or, for a pane narrow
// enough to wrap the draft, a row that IS the tail's end), or a collapsed-paste
// chip on the row. The row may carry a leading prompt glyph (>, ❯, ›) that is
// not part of the payload; the suffix directions both tolerate it.
func classifyComposerRow(normRow string, probe deliveryProbe, tailVisible, chipVisible bool) composerStaged {
	if normRow == "" {
		return composerStagedNone
	}
	if tailVisible {
		trimmed := strings.TrimLeftFunc(normRow, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		if strings.HasSuffix(normRow, probe.completion) ||
			(len([]rune(trimmed)) >= minDistinctiveFragment && strings.HasSuffix(probe.completion, trimmed)) {
			return composerStagedTail
		}
	}
	if chipVisible && strings.Contains(normRow, "[Pasted") {
		return composerStagedChip
	}
	return composerStagedNone
}

// stagedDraftHiddenCursor is the staged check for a pane whose application
// hides the terminal cursor — Claude Code's ordinary composer among them.
// Without a cursor row to anchor on, the check keys off composer STRUCTURE: a
// live composer always repaints a fresh glyph row beneath a submitted prompt's
// echo, so the payload's tail can only be staged input when it ends a row in
// the LAST glyph-anchored block. The block runs while normalized rows stay
// non-empty — a wrapped draft continues below the glyph row, and a box border
// or blank ends the run (footer text below simply fails the row match). A modal
// dialog also ends in glyph rows, but its labels cannot end with this payload's
// tail, so the check stays silent instead of sending Enter into a picker.
func stagedDraftHiddenCursor(pane string, probe deliveryProbe, tailVisible, chipVisible bool) composerStaged {
	rows := strings.Split(strings.TrimSuffix(pane, "\n"), "\n")
	last := -1
	for i, r := range rows {
		if strings.HasPrefix(normalizeDelivery(r), claudeComposerGlyph) {
			last = i
		}
	}
	if last < 0 {
		return composerStagedNone
	}
	for j := last; j < len(rows); j++ {
		normRow := normalizeDelivery(rows[j])
		if normRow == "" {
			break
		}
		if staged := classifyComposerRow(normRow, probe, tailVisible, chipVisible); staged != composerStagedNone {
			return staged
		}
	}
	return composerStagedNone
}

// remedyStrandedSubmit is the #4200 repair: when the prompt is provably still
// staged in the composer after Enter, send ONE more Enter to dispatch it.
// Staged evidence is positive only: the live cursor row ending in this
// payload's tail, or the last composer-glyph block doing so on a hidden-cursor
// pane. The first keystroke demonstrably did not submit — a submitted draft
// leaves the composer input — so the second cannot double-submit this payload;
// worst case it queues behind input the composer is still draining and no-ops
// on the empty composer that results.
//
// The verdict after the remedy is honest about what was observed: a tail-staged
// draft that dispatches was observed delivered (its text rendered AND left the
// composer on our submit); a chip-staged draft that dispatches stays
// sent-unverified, because a placeholder never proved WHICH text it held; a
// draft that survives the remedy Enter downgrades even a landed observation to
// sent-unverified — and so does a remedy whose outcome cannot be re-observed at
// all, because staged evidence was already seen and reporting delivered here is
// the false success #4200 exists to eliminate.
func (t *TmuxSession) remedyStrandedSubmit(probe deliveryProbe, observation deliveryObservation) deliveryObservation {
	if probe.completion == "" {
		return observation
	}
	pasteDeliverySleep(strandedSubmitGrace)
	pane, cursor, ok := t.capturePaneAndCursorState()
	if !ok {
		return observation
	}
	staged := stagedDraftInSnapshot(pane, cursor, probe)
	if staged == composerStagedNone {
		return observation
	}
	stagedPane := pane
	log.WarningLog.Printf("submit: session %q still shows this prompt staged in the composer after Enter; "+
		"the keystroke was swallowed by the still-rendering composer, so sending one more Enter to dispatch the staged draft (#4200)",
		t.sanitizedName)
	boundary, boundaryOK, err := t.sendEnterAndCaptureBoundary()
	if err != nil {
		log.WarningLog.Printf("submit: remedy Enter for session %q did not reach tmux: %v", t.sanitizedName, err)
	} else if boundaryOK {
		// The remedy's boundary is the new delivery boundary: churn after it is
		// the agent responding to the dispatched draft.
		t.seedDeliveryBaseline(boundary)
	} else {
		t.deferDeliveryBaseline()
	}
	pasteDeliverySleep(strandedSubmitSettle)
	pane, cursor, ok = t.capturePaneAndCursorState()
	if !ok {
		// Staged evidence WAS seen and the remedy Enter was sent; whether it
		// dispatched is now unobservable. Returning the earlier observation
		// could report delivered on a prompt last seen unsubmitted — downgrade
		// to the unconfirmed floor instead.
		return deliveryObservation{outcome: deliveryObservedUnverified, pane: stagedPane}
	}
	if stagedDraftInSnapshot(pane, cursor, probe) != composerStagedNone {
		// The draft survived two Enters — a deeper wedge (frozen render, queued
		// input backlog). The paste may have landed, but submission is
		// unconfirmed at best: report unconfirmed, never delivered.
		return deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	}
	if staged == composerStagedTail || observation.outcome == deliveryObservedLanded {
		// The payload itself was seen staged and is gone after our Enter —
		// delivered AND dispatched.
		return deliveryObservation{outcome: deliveryObservedLanded, pane: pane}
	}
	if observation.outcome == deliveryCouldNotObserve {
		// The pane became readable during the remedy: a chip dispatched, but no
		// payload-specific evidence ever rendered. sent-unverified, not
		// could-not-confirm, is the honest report for an observed dispatch of an
		// unverified payload.
		return deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	}
	return observation
}
