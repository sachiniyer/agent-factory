package tmux

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
// transcript echo of a submitted prompt.
const claudeComposerGlyph = "❯"

// stagedEvidence is the set of evidence kinds that are bound to THIS delivery:
// each newly appeared between the pre-paste baseline and the frame tmux
// captured in the same command queue as our Enter. Only bound kinds may
// classify a later frame as staged.
type stagedEvidence struct {
	tail bool
	chip bool
	// chipText is the bound chip's own normalized text ("[PastedContent3207
	// chars]" without spaces). It is set only when chip is bound, and it is
	// what "still staged" must go on looking for: any other chip may be a
	// user's.
	chipText string
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
	bound := stagedEvidence{
		tail: strings.Count(boundaryText, p.completion) > p.completionBaseline,
	}
	// The chip must have grown by EXACTLY one and declare this payload's size:
	// an attached user can paste in the same window, and their chip grows the
	// count exactly like ours, so growth alone cannot attribute a chip. One
	// delivery draws one chip — a larger growth means a concurrent render we
	// cannot attribute — and a chip that does not declare this payload's size
	// ("N chars", "+N lines") is as likely to be the user's still-processing
	// paste as ours (#4530 review).
	if strings.Count(boundaryText, pasteChipMarker)-strings.Count(p.baselineText, pasteChipMarker) == 1 {
		if chip, ok := newestPasteChip(boundaryText); ok && p.chipMatchesPayload(chip) {
			bound.chip = true
			bound.chipText = chip
		}
	}
	return bound
}

// newestPasteChip returns the last "[Pasted ...]" run in a normalized frame —
// the bottom-most chip on the pane. The run must end at its "]": a chip still
// being drawn has no terminator and cannot be sized.
func newestPasteChip(normalized string) (string, bool) {
	i := strings.LastIndex(normalized, pasteChipMarker)
	if i < 0 {
		return "", false
	}
	rest := normalized[i:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return "", false
	}
	return rest[:end+1], true
}

// chipMatchesPayload compares the size a collapsed-paste chip declares against
// this payload's own measured size. Codex prints the pasted character count
// ("[Pasted Content 3207 chars]"); Claude prints the pasted line count
// ("[Pasted text #1 +12 lines]"). Either metric matching is provenance; a chip
// declaring neither — or a different number — cannot be attributed to this
// paste.
func (p deliveryProbe) chipMatchesPayload(chip string) bool {
	if chars, ok := chipDeclaredCount(chip, "chars"); ok {
		return chars == p.pasteRunes
	}
	if lines, ok := chipDeclaredCount(chip, "+", "lines"); ok {
		return lines == p.pasteLines
	}
	return false
}

// chipDeclaredCount reads the integer that precedes a unit word in a
// normalized chip: "…3207chars]" is 3207 chars, "+12lines]" is 12 lines. The
// frame is normalized, so the digits sit directly against the unit.
func chipDeclaredCount(chip string, parts ...string) (int, bool) {
	s := chip
	var num string
	for _, part := range parts {
		if part == "+" {
			i := strings.Index(s, "+")
			if i < 0 {
				return 0, false
			}
			s = s[i+1:]
			continue
		}
		i := strings.Index(s, part)
		if i < 0 {
			return 0, false
		}
		num = s[:i]
	}
	if num == "" {
		return 0, false
	}
	// The digits are the count's trailing run: "#2+12lines" declares 12.
	start := len(num)
	for start > 0 && num[start-1] >= '0' && num[start-1] <= '9' {
		start--
	}
	if start == len(num) {
		return 0, false
	}
	n, err := strconv.Atoi(num[start:])
	return n, err == nil
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

// paneGridRows splits a captured pane into its grid rows, preserving trailing
// blank rows so tmux's 0-based cursor_y still names the same index.
func paneGridRows(pane string) []string {
	return strings.Split(strings.TrimSuffix(pane, "\n"), "\n")
}

// lastComposerGlyphRow is the index of the pane's LAST prompt-glyph row — the
// live composer block's first row. A submitted prompt's echo carries the glyph
// too, but the composer repaints a fresh glyph row beneath it, so the last one
// is the live input's.
func lastComposerGlyphRow(rows []string) int {
	last := -1
	for i, r := range rows {
		if isComposerGlyphRow(normalizeDelivery(r)) {
			last = i
		}
	}
	return last
}

// isComposerGlyphRow reports whether a normalized row opens with a composer
// prompt glyph — Claude's ❯, Codex's ›, Devin's ❭ (task/runner.go), or the
// plain > some composers draw.
func isComposerGlyphRow(norm string) bool {
	return strings.HasPrefix(norm, claudeComposerGlyph) ||
		strings.HasPrefix(norm, "›") || strings.HasPrefix(norm, "❭") ||
		strings.HasPrefix(norm, ">")
}

// bareComposerGlyphRow reports whether a row is an empty composer prompt — the
// bare glyph a composer repaints after a submit, and nothing else. A user's
// ">quote" row is not bare: it carries content.
func bareComposerGlyphRow(row string) bool {
	norm := normalizeDelivery(row)
	return norm == "›" || norm == claudeComposerGlyph || norm == "❭"
}

// blankComposerRow is a row holding nothing but whitespace and the vertical
// edges of a composer box — an empty line INSIDE the input, as opposed to a
// horizontal border, which also normalizes to "" but marks the composer's edge.
func blankComposerRow(raw string) bool {
	return strings.TrimFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || r == '│' || r == '┃' || r == '|'
	}) == ""
}

// composerRegion returns the live composer's grid rows: the glyph-anchored row
// plus the rows below it, stopping at a horizontal border or at the first
// content row that follows a blank gap — the shape of an agent's footer/status
// line ("esc to interrupt" sits a blank row below the composer on real Codex).
// Interior blank rows stay inside the region: a composer whose Enter was
// absorbed as a newline holds exactly those.
func composerRegion(rows []string, anchor int) []string {
	end := len(rows)
	sawBlank := false
	for j := anchor + 1; j < len(rows); j++ {
		norm := normalizeDelivery(rows[j])
		if norm == "" {
			if !blankComposerRow(rows[j]) {
				end = j
				break
			}
			sawBlank = true
			continue
		}
		if sawBlank {
			end = j
			break
		}
	}
	return rows[anchor:end]
}

// boundStillStaged reports whether evidence bound to this delivery still sits
// in the live composer — at or below the pane's last composer-glyph row — in an
// atomic pane+cursor frame. The anchor is positional, not textual: a composer
// the clear+paste could not fully attribute must not read a transcript echo
// (which carries the same tail, and the same chip, above the live composer).
//
// When the grace frame's glyph row moved off the boundary's, the composer was
// repainted. A bare fresh glyph row BELOW the old anchor is a submitted
// composer; anything else — an anchor that moved up, or a content row like a
// user's ">quote" — is a composer shape we can no longer attribute, so the
// draft conservatively still counts as staged.
func boundStillStaged(pane string, probe deliveryProbe, bound stagedEvidence, boundaryAnchor int) bool {
	rows := paneGridRows(pane)
	anchor := lastComposerGlyphRow(rows)
	if anchor < 0 {
		return false
	}
	lower := normalizeDelivery(strings.Join(rows[anchor:], ""))
	if bound.tail && strings.Contains(lower, probe.completion) {
		return true
	}
	if bound.chip && strings.Contains(lower, bound.chipText) {
		return true
	}
	if anchor == boundaryAnchor {
		return false
	}
	return anchor < boundaryAnchor || !bareComposerGlyphRow(rows[anchor])
}

// stillChipComposer is the stillness+position proof for the one remediable
// shape: the boundary's composer region held exactly one content row — the
// bound chip on the composer glyph row — and the grace frame's composer region
// is byte-identical, plus at most the one whitespace-only row an absorbed
// Enter can append. Anything else stands down: a literal draft (tail-bound)
// never reaches here, a second chip or user text adds a content row, a moved
// anchor means a repainted composer, and a footer redrawn inside the region
// changes the bytes. The visible cursor must rest where undisturbed input left
// it: on the chip row at its insert or content-end column, or on the absorbed
// Enter's blank row at the insert column. A hidden cursor (Claude's ordinary
// composer draws one) cannot prove that, so it withholds.
func stillChipComposer(boundary, pane string, cursor paneCursorState, chipText string) bool {
	if !cursor.Visible {
		return false
	}
	bRows := paneGridRows(boundary)
	gRows := paneGridRows(pane)
	anchor := lastComposerGlyphRow(bRows)
	if anchor < 0 || anchor >= len(gRows) || lastComposerGlyphRow(gRows) != anchor {
		return false
	}
	bRegion := composerRegion(bRows, anchor)
	gRegion := composerRegion(gRows, anchor)
	// The boundary composer's one content row must be the bound chip itself.
	// Strip exactly one leading prompt glyph, not one of each — the payload
	// itself may open with '>'.
	chipRow := normalizeDelivery(bRegion[0])
	if r, size := utf8.DecodeRuneInString(chipRow); r == '❯' || r == '›' || r == '❭' || r == '>' {
		chipRow = chipRow[size:]
	}
	if chipRow != chipText || !singleContentRow(bRegion) {
		return false
	}
	// Stillness: every boundary region row must appear, in order, in the grace
	// region; the grace region may add at most one row, and only whitespace.
	if len(gRegion) > len(bRegion)+1 {
		return false
	}
	i := 0
	for _, row := range gRegion {
		if i < len(bRegion) && row == bRegion[i] {
			i++
			continue
		}
		if !blankComposerRow(row) {
			return false
		}
	}
	if i != len(bRegion) {
		return false
	}
	// Position: the cursor must sit inside the region, parked where this draft
	// alone could have left it.
	if cursor.Row < anchor || cursor.Row >= anchor+len(gRegion) || cursor.Row >= len(gRows) {
		return false
	}
	insert := firstContentCol(gRows[anchor])
	if blankComposerRow(gRows[cursor.Row]) {
		return insert >= 0 && cursor.Col == insert
	}
	return cursor.Row == anchor &&
		(cursor.Col == insert || cursor.Col == contentEndCol(gRows[anchor]))
}

// singleContentRow reports whether a region holds exactly one row that is not
// whitespace-only — for the remediable shape, the chip row itself.
func singleContentRow(region []string) bool {
	content := 0
	for _, row := range region {
		if !blankComposerRow(row) && normalizeDelivery(row) != "" {
			content++
		}
	}
	return content == 1
}

// firstContentCol is the column of a row's first character that can be
// composer content — anything but whitespace, box drawing, or a prompt glyph —
// or -1 when the row is pure decoration.
func firstContentCol(row string) int {
	col := 0
	for _, r := range row {
		if unicode.IsSpace(r) || (r >= 0x2500 && r <= 0x259F) ||
			r == '❯' || r == '›' || r == '>' || r == '|' {
			col++
			continue
		}
		return col
	}
	return -1
}

// contentEndCol is the column just past a row's last drawn character — where
// an app parks the cursor at the end of what it rendered. Trailing whitespace
// and the box's right edge are not content.
func contentEndCol(row string) int {
	end := -1
	col := 0
	for _, r := range row {
		if !unicode.IsSpace(r) && !(r >= 0x2500 && r <= 0x259F) {
			end = col + 1
		}
		col++
	}
	return end
}

// remedyStrandedSubmit is the #4200 repair, narrowed to the one shape whose
// identity is provable: a composer still rendering a paste can absorb Enter as
// a literal newline, leaving the whole prompt staged and unsubmitted while the
// pre-Enter observation already reads landed. After a short grace it looks
// once more and, when the draft provably is that one shape, sends ONE more
// Enter — never a re-paste. Every other staged shape stands down and reports
// sent-unverified.
//
// Three conditions authorize the remedy Enter, and each is necessary:
//
//  1. Binding. The evidence newly appeared between the pre-paste baseline and
//     the frame captured in the same tmux command queue as our Enter
//     (boundEvidence), and for a chip it must declare this payload's size.
//     Evidence that predates the paste, or a second chip in the same window,
//     cannot bind — whoever drew it.
//  2. Stillness. The grace frame's composer region is byte-identical to the
//     boundary's, modulo the one whitespace row an absorbed Enter appends
//     (stillChipComposer). Any keystroke, whitespace edit, user paste, footer
//     redraw, or moved anchor changes those bytes. This is what makes a chip
//     usable at all: the agent draws the same chip for a user's paste and for
//     a transcript echo, but neither can appear in a frame that has not
//     changed.
//  3. Position. The bound evidence sits in the live composer — at or below the
//     pane's LAST composer-glyph row (boundStillStaged) — and the cursor is
//     visible and parked where this draft alone could have left it. A
//     transcript echo cannot satisfy position: the composer repaints its own
//     glyph row beneath the echo.
//
// What the conditions do not cover: the gap between the grace capture and the
// remedy keystroke is check-then-act, like absenceStillProven's. The daemon's
// attach defer (#1586) is the guard for a user typing in a pane.
//
// The verdict never rounds up. Bound evidence that stays staged, a pane that
// changed while staged, a remedy whose outcome cannot be re-read, and every
// non-chip staged shape all report sent-unverified. Only a draft that left the
// composer keeps the observation it had before Enter.
func (t *TmuxSession) remedyStrandedSubmit(probe deliveryProbe, observation deliveryObservation, boundary string, boundaryOK bool) deliveryObservation {
	if !boundaryOK {
		return observation
	}
	stripped := xansi.Strip(boundary)
	bound := probe.boundEvidence(normalizeDelivery(stripped))
	if !bound.any() {
		return observation
	}
	boundaryAnchor := lastComposerGlyphRow(paneGridRows(stripped))

	pasteDeliverySleep(strandedSubmitGrace)
	pane, cursor, ok := t.capturePaneAndCursorState()
	if !ok {
		return observation
	}
	unverified := deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	if !boundStillStaged(pane, probe, bound, boundaryAnchor) {
		return observation
	}

	// The draft is still staged. Only the bound chip in a byte-still composer
	// authorizes the remedy Enter; every other staged shape — a literal draft,
	// foreign content, a moved or hidden cursor — stands down.
	if !bound.chip || !stillChipComposer(stripped, pane, cursor, bound.chipText) {
		log.WarningLog.Printf("submit: session %q still shows this prompt's bound evidence staged in the composer after Enter, "+
			"but it is not a still, single-chip composer this delivery can prove is only ours; "+
			"withholding the remedy Enter and reporting sent-unverified (#4200). Pane tail: %s",
			t.sanitizedName, oneLineTail(pane))
		return unverified
	}

	log.WarningLog.Printf("submit: session %q still holds this prompt's bound chip staged in a byte-identical composer after Enter; "+
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

	// Submitted means the draft LEFT: a settle read must find the bound
	// evidence gone from the live composer. A composer that absorbed the
	// remedy Enter too still shows it — a deeper wedge, never delivered.
	pasteDeliverySleep(strandedSubmitSettle)
	settled, _, ok := t.capturePaneAndCursorState()
	if !ok || boundStillStaged(settled, probe, bound, boundaryAnchor) {
		return unverified
	}
	return deliveryObservation{outcome: observation.outcome, pane: settled}
}
