package tmux

import "strings"

// StagedDraftRead is the verdict of one staged-draft recheck (#7545): where
// the bound evidence of a session's last prompt send sits in the pane now.
// The daemon's staged-draft watch folds these across polls; the contract is
// deliberately closed so an answer is always one of five things, never a
// guess about prompt text.
type StagedDraftRead int

const (
	// StagedDraftNoEvidence means the last send bound no composer evidence —
	// a send whose boundary captured nothing attributable, or none since the
	// session was (re)attached. There is nothing the recheck may ever match,
	// so the watch ends without a single capture.
	StagedDraftNoEvidence StagedDraftRead = iota
	// StagedDraftUnreadable means the pane could not be judged this tick —
	// the capture failed, or no composer-glyph row is visible (a dialog or a
	// non-composer screen). It is a blind read, never a negative: it neither
	// resets a sustain window nor resolves the watch.
	StagedDraftUnreadable
	// StagedDraftGone means the frame was readable and the bound evidence is
	// nowhere in it — submitted, cleared, or scrolled past. The send's watch
	// ends; nothing remains to flag.
	StagedDraftGone
	// StagedDraftSubmitted means the bound evidence sits ABOVE the pane's
	// last composer-glyph row — a transcript echo, so the send did submit and
	// the sent-unverified report was only unproven, not wrong. The watch ends
	// here too; this is the common delivered-but-unverified resolution.
	StagedDraftSubmitted
	// StagedDraftStaged means the bound evidence sits in the live composer —
	// the composer region anchored at the pane's last composer-glyph row —
	// so the draft the send pasted is still unsubmitted in an otherwise
	// readable composer.
	StagedDraftStaged
)

// stagedDraftEvidence is the provable residue of one submit attempt, kept on
// the TmuxSession so the daemon's staged-draft recheck can ask about it later
// (#7545). Each string is one of the two evidence kinds boundEvidence may bind
// to this delivery — the payload's normalized completion tail, or the bound
// paste chip's own normalized text — and an empty field means that kind did
// not bind. Only bound strings are ever stored, and only while their send is
// the latest: nothing here is a text match on arbitrary composer contents,
// which is what keeps a user's own typed draft — even an identical one — from
// ever flagging as af's. Memory-only by design (#7545 decision): a daemon
// restart forgets sends it did not watch, rather than putting prompt-derived
// content on a durable row.
type stagedDraftEvidence struct {
	tail string
	chip string
}

// BoundDraftStillStaged reports where the last send's bound evidence sits in
// the pane RIGHT NOW. It is the staged-draft watch's one pane read: called at
// most once per poll, only for a session whose recorded delivery still says
// sent-unverified, and it spends a capture only when evidence was stashed —
// the common sent-unverified send that submitted fine answers StagedDraft-
// Submitted (or Gone) on its first read and is never asked again. The pane is
// captured with capturePaneForDelivery's convention — `capture-pane -p`,
// physical grid rows — because the composer geometry helpers it feeds are the
// same ones stagedReading trusted at the Enter boundary.
//
// The check is positional, never textual: evidence above the last composer-
// glyph row is a submitted echo, evidence inside the composer region — the
// same region boundEvidence searched when the send bound it — is the
// still-staged draft, and evidence anywhere else — or a frame with no
// composer at all — is no claim at all. The read runs outside inputMu; a
// recheck concurrent with an in-flight send sees at worst a one-tick skew
// (old evidence against a mid-send frame), which the next poll's verdict
// corrects once the send's own attempt record lands.
func (t *TmuxSession) BoundDraftStillStaged() (StagedDraftRead, string) {
	ev := t.stagedDraft.Load()
	if ev == nil || (ev.tail == "" && ev.chip == "") {
		return StagedDraftNoEvidence, "the last send bound no composer evidence"
	}
	pane, ok := t.capturePaneForDelivery()
	if !ok {
		return StagedDraftUnreadable, "the pane could not be captured"
	}
	return stagedDraftReading(pane, *ev)
}

// stagedDraftReading classifies one frame against stashed evidence. The
// anchor is the pane's LAST composer-glyph row — the live composer repaints
// its own glyph beneath every transcript echo — so "above the anchor" is
// submitted and the composer region it opens is the live draft. The composer
// check runs first: a send whose echo reached the transcript while its draft
// also sits in the composer is still staged, and the flag is the honest
// answer for it.
func stagedDraftReading(pane string, ev stagedDraftEvidence) (StagedDraftRead, string) {
	rows := paneGridRows(pane)
	anchor := lastComposerGlyphRow(rows)
	if anchor < 0 {
		return StagedDraftUnreadable, "no composer-glyph row is visible"
	}
	// The staged check searches the composer region — never every row below
	// the glyph: the status/footer line under the composer is not the draft,
	// and a bound tail that appears only there ("bypass permissions on"
	// under a short "on" send) must not read staged (Codex #7547). But
	// composerRegion also stops at the first content row after an interior
	// blank, and a draft that wraps past a blank line is textually the same
	// shape as a footer — so the frame's own border makes the call, exactly
	// the way draftClosedByFrame judges it for ComposerDraftPresent: a hard
	// border below the stranded content proves it interior (a multiline
	// draft's tail must not read Gone and close the watch), no border proves
	// it the footer.
	regionEnd := anchor + len(composerRegion(rows, anchor))
	if draftClosedByFrame(rows, anchor) {
		for k := regionEnd; k < len(rows); k++ {
			if composerHardBorder(rows[k]) {
				break
			}
			regionEnd = k + 1
		}
	}
	region := normalizeDelivery(strings.Join(rows[anchor:regionEnd], ""))
	switch {
	case ev.tail != "" && strings.Contains(region, ev.tail):
		return StagedDraftStaged, "the send's bound text is still in the live composer"
	case ev.chip != "" && strings.Contains(region, ev.chip):
		return StagedDraftStaged, "the send's bound paste chip is still in the live composer"
	}
	upper := normalizeDelivery(strings.Join(rows[:anchor], ""))
	switch {
	case ev.tail != "" && strings.Contains(upper, ev.tail):
		return StagedDraftSubmitted, "the send's bound text reads as a submitted echo"
	case ev.chip != "" && strings.Contains(upper, ev.chip):
		return StagedDraftSubmitted, "the send's bound paste chip reads as a submitted echo"
	}
	return StagedDraftGone, "the send's bound evidence is nowhere in the frame"
}
