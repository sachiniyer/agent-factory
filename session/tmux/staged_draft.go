package tmux

import "strings"

// stagedDraftHistoryBound caps how far the staged-draft recheck reaches back
// through scrollback — the -S bound on every capture it makes (Codex #7547,
// fourth round). A staged verdict may stand on an absent echo only when the
// whole transcript since the send's boundary is in view, and that reach is
// bounded, not "all history".
const stagedDraftHistoryBound = 2000

// stagedBoundaryMark turns the transcript that sat above the send's composer
// into the boundary's line — the recheck's completeness witness: every
// post-boundary row is newer than it, so while it survives the scrollback-
// inclusive capture, nothing since the boundary can be missing. It is the
// normalized TAIL of that transcript (bounded, rune-safe), and it is empty
// when the transcript was too small to identify a line — fewer than 16
// normalized runes could collide with ordinary output — in which case the
// recheck's history-start rule is the proof instead.
func stagedBoundaryMark(bupper string) string {
	const maxRunes, minRunes = 160, 16
	rs := []rune(bupper)
	if len(rs) < minRunes {
		return ""
	}
	if len(rs) > maxRunes {
		rs = rs[len(rs)-maxRunes:]
	}
	return string(rs)
}

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
// baseline a transcript must beat to prove THIS send echoed. boundaryMark is
// the normalized tail of that same above-anchor transcript — the boundary's
// line: every post-boundary row is newer than it, so while it survives in a
// scrollback-inclusive recheck nothing printed since the boundary can be
// missing (tmux evicts oldest-first). Only bound strings are ever stored,
// and only while their send is the latest.
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
	tail         string
	tailWhole    bool
	tailEcho     int
	chip         string
	chipEcho     int
	boundaryMark string
}

// BoundDraftStillStaged reports where the last send's bound evidence sits in
// the pane RIGHT NOW. It is the staged-draft watch's one pane read: called at
// most once per poll, only for a session whose recorded delivery still says
// sent-unverified, and it spends a capture only when evidence was stashed —
// the common sent-unverified send that submitted fine answers StagedDraft-
// Submitted (or Gone) on its first read and is never asked again. The pane is
// captured with capturePaneForDeliveryHistory — `capture-pane -p -S -<bound>`,
// scrollback then physical grid rows — because a staged verdict may stand on
// an absent echo only when every row printed since the boundary is in view
// (Codex #7547, fourth round); the composer geometry helpers it feeds are
// the same ones stagedReading trusted at the Enter boundary.
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
	pane, ok := t.capturePaneForDeliveryHistory()
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
	anchor := stagedLastComposerAnchor(rows)
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
	// The region's end is proven only when a boundary marker closed it — a
	// border row, or the blank-gap rule composerRegion itself applies. When
	// the rows below the anchor are contiguous content to the pane's last
	// row, nothing separated the composer from a footer glued to it
	// (codexWarningPane's blankBeforeFooter=false shape), and the literal
	// text comparisons below would weigh footer text against the bound
	// tail. Master's own ruling for that shape withholds rather than
	// resolves (stillChipComposer's "not only the bound chip"), so the
	// recheck reads it Unreadable — never Gone, which would close a live
	// watch on a genuinely staged draft (Codex #7547). The chip's Contains
	// match is unaffected: a status line cannot hold this payload's bound
	// paste chip.
	regionBounded := true
	if regionEnd == len(rows) {
		for j := anchor + 1; j < regionEnd; j++ {
			if normalizeDelivery(rows[j]) != "" {
				regionBounded = false
				break
			}
		}
	}
	// The composer's text after its prompt glyph. A payload short enough to
	// be its own bound tail must EQUAL it — "yes" sent must not read staged
	// inside a user's "they established" (Codex #7547 independent review).
	// A longer payload's bound tail is its END, so the same claim for a long
	// send is a suffix match: user text appended after the paste breaks it,
	// exactly the way an edit breaks the short-payload equality.
	composerText := stagedComposerText(region)
	tailStaged := ev.tail != "" && regionBounded &&
		(ev.tailWhole && composerText == ev.tail || !ev.tailWhole && strings.HasSuffix(composerText, ev.tail))
	chipStaged := ev.chip != "" && strings.Contains(region, ev.chip)
	ambiguous := stagedAnchorAmbiguous(rows, anchor)
	upperRows := rows[:anchor]
	upper := normalizeDelivery(strings.Join(upperRows, ""))
	tailEchoes, chipEchoes := 0, 0
	if ev.tail != "" {
		tailEchoes = strings.Count(upper, ev.tail)
	}
	if ev.chip != "" {
		chipEchoes = strings.Count(upper, ev.chip)
	}
	// Staged may stand on an ABSENT echo only when everything printed since
	// the boundary is in view — otherwise af's own echo could have scrolled
	// out of reach, and a user's identical recall or retype would flag and
	// invite an Enter that re-runs a finished instruction (Codex #7547,
	// fourth round). The boundary mark is the tail of the transcript that
	// sat above the send's composer: tmux history evicts oldest-first, so
	// every post-boundary row is newer than it — while it survives in the
	// scrollback-inclusive capture, nothing since the boundary is missing.
	// A boundary with no transcript above it needs no mark: reaching the
	// -S bound's start — fewer rows above the anchor than the bound —
	// already shows the whole post-boundary output.
	boundaryReached := ev.boundaryMark == "" && len(upperRows) < stagedDraftHistoryBound ||
		ev.boundaryMark != "" && strings.Contains(upper, ev.boundaryMark)
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
		case !boundaryReached:
			// The transcript moved past what the capture can prove — the
			// echo count is incomplete and may be hiding the send's own
			// scrolled-off echo. Blind, never a flag: the blind-read
			// budget retires the watch (Codex #7547, fourth round).
			return StagedDraftUnreadable, "the transcript scrolled beyond the bounded history — the echo count is incomplete"
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
		case !boundaryReached:
			return StagedDraftUnreadable, "the transcript scrolled beyond the bounded history — the echo count is incomplete"
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
	if !regionBounded {
		return StagedDraftUnreadable, "the composer's end is not bounded — a footer may sit inside the region"
	}
	return StagedDraftGone, "the send's bound evidence is nowhere in the frame"
}

// stagedComposerGlyphRow is the recheck's composer-glyph set — the shared
// readers' set PLUS Codex's Ultra-tier » (codexComposerGlyphs). The submit
// path deliberately leaves » unrecognized (composer_draft.go: an echoed » is
// indistinguishable from the live composer there, so non-placeholder text
// proves nothing). The recheck makes that distinction a different way — the
// last glyph row anchors the live composer, and the echo baselines bound at
// send time attribute what sits above it — so the wider set only widens what
// the watch can prove. It lives here, not in the shared readers, because
// master's #4200 tests pin their narrower set (Codex #7547 review).
func stagedComposerGlyphRow(norm string) bool {
	return isComposerGlyphRow(norm) || strings.HasPrefix(norm, "»")
}

// stagedLastComposerAnchor is lastComposerGlyphRow over the recheck's glyph
// set: the pane's last prompt-glyph row, » included.
func stagedLastComposerAnchor(rows []string) int {
	last := -1
	for i, r := range rows {
		if stagedComposerGlyphRow(normalizeDelivery(r)) {
			last = i
		}
	}
	return last
}

// stagedComposerText is afterComposerGlyph over the recheck's glyph set —
// strips exactly one leading prompt glyph, » included.
func stagedComposerText(norm string) string {
	if rest, ok := strings.CutPrefix(norm, "»"); ok {
		return rest
	}
	return afterComposerGlyph(norm)
}

// stagedAnchorAmbiguous is composerAnchorAmbiguous over the recheck's glyph
// set — a »-leading draft line makes the anchor ambiguous exactly the way a
// `>` quote line does.
func stagedAnchorAmbiguous(rows []string, anchor int) bool {
	for j := anchor - 1; j >= 0; j-- {
		norm := normalizeDelivery(rows[j])
		if norm == "" || !stagedComposerGlyphRow(norm) {
			return false
		}
		if stagedComposerText(norm) != "" {
			return true
		}
	}
	return false
}
