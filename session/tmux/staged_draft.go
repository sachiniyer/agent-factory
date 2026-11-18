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
// (#7545). tail and chip are the two evidence kinds boundEvidence may bind to
// a delivery. tailWhole records that the bound tail IS the whole normalized
// payload (32 runes or fewer), and tailEcho/chipEcho count how many copies of
// each kind were already visible above the composer at the boundary — the
// baseline a transcript must beat to prove THIS send echoed. Only bound
// strings are ever stored, and only while their send is the latest.
//
// The strings alone do not make a draft af's, and the recheck never lets
// them: Staged is answered only for a frame af's still-unsubmitted paste
// ALONE explains — a whole-payload tail must equal the composer text, not be
// contained in it, and any identical echo visible above the anchor demotes
// the read, because a user retyping or up-arrow-recalling the very text the
// transcript shows produces the same composer contents (Codex #7547
// independent review). Memory-only by design (#7545 decision): a daemon
// restart forgets sends it did not watch, rather than putting prompt-derived
// content on a durable row.
type stagedDraftEvidence struct {
	tail      string
	tailWhole bool
	tailEcho  int
	chip      string
	chipEcho  int
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
// The check is positional and baseline-bound, never a bare text match:
// evidence inside the composer region — the same region boundEvidence
// searched when the send bound it — is the still-staged draft ONLY when no
// echo above the anchor can also explain it (a grown echo is the send's own
// submission, a pre-existing one makes the composer copy unattributable —
// Codex #7547 independent review); evidence above the last composer-glyph
// row alone is a submitted echo; evidence nowhere — or a frame with no
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
// its own glyph beneath every transcript echo — so "above the anchor" is the
// transcript and the composer region it opens is the live draft.
//
// A composer-region match is Staged only when af's still-unsubmitted paste
// ALONE explains it (Codex #7547 independent review): the send's own text
// echoing above the anchor too, or an identical echo that predates the send,
// means the composer copy could be the user's — up-arrow re-issues verbatim
// what the transcript shows, so echo evidence demotes the match to
// Submitted (the echo grew since the boundary: this send provably landed) or
// Unreadable (it predates the send, or the anchor itself is ambiguous).
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
	// The composer's text after its prompt glyph. A payload short enough to
	// be its own bound tail must EQUAL it — "yes" sent must not read staged
	// inside a user's "they established" (Codex #7547 independent review).
	// A longer payload's bound tail is its END, so the same claim for a long
	// send is a suffix match: user text appended after the paste breaks it,
	// exactly the way an edit breaks the short-payload equality.
	composerText := afterComposerGlyph(region)
	tailStaged := ev.tail != "" &&
		(ev.tailWhole && composerText == ev.tail || !ev.tailWhole && strings.HasSuffix(composerText, ev.tail))
	chipStaged := ev.chip != "" && strings.Contains(region, ev.chip)
	ambiguous := composerAnchorAmbiguous(rows, anchor)
	upper := normalizeDelivery(strings.Join(rows[:anchor], ""))
	tailEchoes, chipEchoes := 0, 0
	if ev.tail != "" {
		tailEchoes = strings.Count(upper, ev.tail)
	}
	if ev.chip != "" {
		chipEchoes = strings.Count(upper, ev.chip)
	}
	switch {
	case tailStaged:
		switch {
		case ambiguous && (tailEchoes > 0 || ev.tailEcho > 0):
			// A `>`-leading quote inside a multiline draft reads as the
			// composer glyph (#6817's ambiguity): the rows above the anchor
			// could be draft text, so the echo count cannot be trusted to
			// decide between submitted and staged.
			return StagedDraftUnreadable, "the last composer glyph may be a quote line inside the draft — the anchor is ambiguous"
		case tailEchoes > ev.tailEcho:
			// The transcript gained a copy of this send's text since the
			// boundary: the send landed, and the matching composer copy is
			// the user's — a recall or a retype — never af's to flag.
			return StagedDraftSubmitted, "the send's bound text echoed into the transcript since the boundary"
		case tailEchoes > 0 || ev.tailEcho > 0:
			// An identical echo predates the send (or one scrolled off
			// since it bound): the composer match could be the user's own
			// recall of it. Unprovable either way — never a negative.
			return StagedDraftUnreadable, "an identical echo predates the send — the composer copy may be the user's own recall"
		default:
			return StagedDraftStaged, "the send's bound text is still in the live composer"
		}
	case chipStaged:
		switch {
		case ambiguous && (chipEchoes > 0 || ev.chipEcho > 0):
			return StagedDraftUnreadable, "the last composer glyph may be a quote line inside the draft — the anchor is ambiguous"
		case chipEchoes > ev.chipEcho:
			return StagedDraftSubmitted, "the send's bound paste chip echoed into the transcript since the boundary"
		case chipEchoes > 0 || ev.chipEcho > 0:
			return StagedDraftUnreadable, "an identical chip echo predates the send — the composer copy may be the user's own recall"
		default:
			return StagedDraftStaged, "the send's bound paste chip is still in the live composer"
		}
	}
	// No provable staged match. With a trustworthy anchor the send may still
	// have landed without leaving a draft: an echo count that grew since the
	// boundary is this send's transcript. With an ambiguous anchor the
	// "upper" rows may be draft text, so even that is unprovable.
	if ambiguous {
		return StagedDraftUnreadable, "the last composer glyph may be a quote line inside the draft — the anchor is ambiguous"
	}
	switch {
	case ev.tail != "" && tailEchoes > ev.tailEcho:
		return StagedDraftSubmitted, "the send's bound text reads as a submitted echo"
	case ev.chip != "" && chipEchoes > ev.chipEcho:
		return StagedDraftSubmitted, "the send's bound paste chip reads as a submitted echo"
	}
	return StagedDraftGone, "the send's bound evidence is nowhere in the frame"
}
