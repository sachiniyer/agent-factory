package tmux

import (
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

// promptStagedAtCursor reports whether this delivery's prompt is still visibly
// staged in the composer at the live input cursor — the one position where
// visible text is provably unsubmitted input. Anchoring on the cursor row is
// what separates a stranded draft from a submitted one: a dispatched prompt's
// echo may keep the same tail visible in the transcript, but the cursor row
// itself then holds the fresh empty composer.
//
// Two sightings qualify: the payload's completion tail ending the cursor row
// (or, for a pane narrow enough to wrap the draft, a cursor row that IS the
// tail's end), and a collapsed-paste chip on the cursor row. Everything else —
// an empty row, a hidden cursor, an unreadable cursor query — is not evidence,
// so the remedy never fires on a pane that might have submitted already.
func (t *TmuxSession) promptStagedAtCursor(pane string, probe deliveryProbe) composerStaged {
	normalized := normalizeDelivery(pane)
	tailVisible := probe.completion != "" && strings.Contains(normalized, probe.completion)
	chipVisible := strings.Contains(normalized, "[Pasted")
	if !tailVisible && !chipVisible {
		return composerStagedNone
	}
	cursor, err := t.readPaneCursorState()
	if err != nil || !cursor.Visible {
		return composerStagedNone
	}
	row, ok := paneRowAt(pane, cursor.Row)
	if !ok {
		return composerStagedNone
	}
	normRow := normalizeDelivery(row)
	if normRow == "" {
		return composerStagedNone
	}
	if tailVisible {
		// The row may carry a leading prompt glyph (>, ❯, ›) that is not part
		// of the payload; the suffix directions below both tolerate it.
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

// remedyStrandedSubmit is the #4200 repair: when the prompt is provably still
// staged at the composer cursor after Enter, send ONE more Enter to dispatch it.
// The first keystroke demonstrably did not submit — a submitted draft leaves the
// cursor row — so the second cannot double-submit this payload; worst case it
// queues behind an input the composer is still draining and no-ops on the empty
// composer that results.
//
// The verdict after the remedy is honest about what was observed: a tail-staged
// draft that dispatches was observed delivered (its text rendered AND left the
// composer on our submit); a chip-staged draft that dispatches stays
// sent-unverified, because a placeholder never proved WHICH text it held; and a
// draft that survives the remedy Enter downgrades even a landed observation to
// sent-unverified — the paste reached the pane, but the prompt never provably
// reached the agent, and reporting delivered here is the false success #4200
// exists to eliminate.
func (t *TmuxSession) remedyStrandedSubmit(probe deliveryProbe, observation deliveryObservation) deliveryObservation {
	if probe.completion == "" {
		return observation
	}
	pasteDeliverySleep(strandedSubmitGrace)
	pane, ok := t.capturePaneForDelivery()
	if !ok {
		return observation
	}
	staged := t.promptStagedAtCursor(pane, probe)
	if staged == composerStagedNone {
		return observation
	}
	log.WarningLog.Printf("submit: session %q still shows this prompt staged at the composer cursor after Enter; "+
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
	pane, ok = t.capturePaneForDelivery()
	if !ok {
		return observation
	}
	if t.promptStagedAtCursor(pane, probe) != composerStagedNone {
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
