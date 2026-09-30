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
		chip: p.boundPasteChip(boundaryText),
	}
}

// boundPasteChip reports whether the chip count grew by EXACTLY one between the
// baseline and the Enter boundary, and whether that chip declares this
// payload's size. Both halves are provenance (#4530 review): an attached user
// can paste in the same window, and their chip grows the count exactly like
// ours, so growth alone cannot attribute the chip. One delivery draws one
// chip — a larger growth means a concurrent render we cannot attribute — and a
// chip that does not declare this payload's size ("N chars", "+N lines") is as
// likely to be the user's still-processing paste as ours. A chip with no
// readable size cannot exclude concurrent input either, so it does not bind.
func (p deliveryProbe) boundPasteChip(boundaryText string) bool {
	growth := strings.Count(boundaryText, pasteChipMarker) - strings.Count(p.baselineText, pasteChipMarker)
	if growth != 1 {
		return false
	}
	chip, ok := newestPasteChip(boundaryText)
	return ok && p.chipMatchesPayload(chip)
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
// In both geometries the tail is read across wrapped rows: a literal draft
// wider than the pane can leave any length of text on its last visual row.
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
		var block strings.Builder
		for j := last; j < len(rows); j++ {
			norm := normalizeDelivery(rows[j])
			if norm == "" {
				if blankComposerRow(rows[j]) {
					continue
				}
				return false
			}
			block.WriteString(norm)
			if composerRowHolds(norm, probe, bound) ||
				(bound.tail && strings.HasSuffix(block.String(), probe.completion)) {
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
	if composerRowHolds(normalizeDelivery(rows[row]), probe, bound) {
		return true
	}
	return bound.tail && strings.HasSuffix(wrappedTextEndingAt(rows, row, len(probe.completion)), probe.completion)
}

// wrappedTextEndingAt joins the normalized text of the contiguous non-empty
// rows ending at row, reading upward until it holds at least want bytes. A
// literal draft wider than the pane wraps, and its last visual row can be any
// length — the #4530 play-test stranded a draft whose last row was "R_4530",
// too short to be matched on its own — so the tail must be read across the
// wrap. The walk stops at an empty row (a blank line or a border), so it never
// joins text across the composer's edge.
func wrappedTextEndingAt(rows []string, row, want int) string {
	joined := ""
	for k := row; k >= 0 && len(joined) < want; k-- {
		norm := normalizeDelivery(rows[k])
		if norm == "" {
			break
		}
		joined = norm + joined
	}
	return joined
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

// isComposerGlyphRow reports whether a normalized row opens with a composer
// prompt glyph — Claude's ❯, Codex's ›, or the plain > some composers draw.
// The composer's first row carries it; so does a submitted prompt's echo, so
// only the LAST such row can anchor the live composer.
func isComposerGlyphRow(norm string) bool {
	return strings.HasPrefix(norm, claudeComposerGlyph) ||
		strings.HasPrefix(norm, "›") || strings.HasPrefix(norm, ">")
}

// composerBlockBounds locates the live composer region in a pane's rows: from
// the LAST prompt-glyph row down to the row before the next horizontal border
// (or the frame's end). The last glyph row is the composer's own first row —
// a submitted prompt's glyph'd echo is always above it, because the composer
// repaints beneath the echo. found is false when no glyph row exists at all;
// callers then cannot attribute any row to the composer.
func composerBlockBounds(rows []string) (first, end int, ok bool) {
	first = -1
	for i, r := range rows {
		if isComposerGlyphRow(normalizeDelivery(r)) {
			first = i
		}
	}
	if first < 0 {
		return 0, 0, false
	}
	end = len(rows)
	for j := first + 1; j < len(rows); j++ {
		if normalizeDelivery(rows[j]) == "" && !blankComposerRow(rows[j]) {
			end = j
			break
		}
	}
	return first, end, true
}

// composerBlockText joins the normalized text of the composer block: the last
// glyph row plus its continuation rows, across blank continuation rows.
func composerBlockText(rows []string) (string, bool) {
	first, end, ok := composerBlockBounds(rows)
	if !ok {
		return "", false
	}
	var block strings.Builder
	for j := first; j < end; j++ {
		block.WriteString(normalizeDelivery(rows[j]))
	}
	return block.String(), true
}

// composerHoldsBoundEvidence reports whether the live composer region still
// contains this delivery's bound evidence — the completion tail, or the bound
// paste chip. stagedInComposer answers whether the evidence sits at the draft's
// insertion point; this answers only whether it is still INSIDE the composer at
// all, which is what decides "still staged, now mixed with foreign text" apart
// from "submitted" (#4530 review).
func composerHoldsBoundEvidence(pane string, probe deliveryProbe, bound stagedEvidence) bool {
	block, ok := composerBlockText(strings.Split(strings.TrimSuffix(pane, "\n"), "\n"))
	if !ok {
		return false
	}
	if bound.tail && strings.Contains(block, probe.completion) {
		return true
	}
	return bound.chip && strings.Contains(block, pasteChipMarker)
}

// composerRenderIsOursOnly reports that the composer block renders THIS
// delivery and nothing else: the bound tail must reconstruct the whole block —
// the payload plus its leading glyph, across however many rows the draft
// wrapped to — and the bound chip must be the block's one and only row of
// content. Anything else in the block (a stale draft C-u could not clear, an
// attached user's typed or pasted text between baseline and boundary) means
// the composer no longer holds only what our Enter was sent to submit (#4530
// review). The check sits at the grace read, so input arriving between the
// boundary and grace is excluded by frameStill first; this covers the
// pre-boundary window stillness cannot see.
func composerRenderPure(pane string, probe deliveryProbe, bound stagedEvidence) bool {
	block, ok := composerBlockText(strings.Split(strings.TrimSuffix(pane, "\n"), "\n"))
	if !ok {
		return false
	}
	// The block's first row carries one prompt glyph; everything after it is
	// payload render. Strip exactly one leading rune, not one of each glyph —
	// the payload itself may open with '>'.
	if r, size := utf8.DecodeRuneInString(block); r == '❯' || r == '›' || r == '>' {
		block = block[size:]
	}
	if bound.tail && block == probe.payload {
		return true
	}
	// A chip render is exactly one row: the chip is the block's whole text.
	return bound.chip && strings.HasPrefix(block, pasteChipMarker) &&
		strings.Count(block, pasteChipMarker) == 1 && strings.HasSuffix(block, "]")
}

// composerCursorIsHome reports whether the visible cursor rests where a
// composer that drew nothing but our payload would leave it: at the block's
// text-insert column when it sits on a blank continuation row (a swallowed
// Enter parks it there), or at the content end of a content row. Whitespace is
// invisible to the normalized stillness check — a user typing a space on a
// continuation row draws no new normalized text — but it always moves the
// cursor past where our own input could have left it (#4530 review). A pane
// whose cursor cannot be measured carries no such signal, so it neither passes
// nor vetoes: the check reports true and the residual stays with the glyph-less
// and hidden-cursor geometries, which have no column to compare against.
func composerCursorIsHome(pane string, cursor paneCursorState) bool {
	if !cursor.Visible {
		return true
	}
	rows := strings.Split(strings.TrimSuffix(pane, "\n"), "\n")
	if cursor.Row < 0 || cursor.Row >= len(rows) {
		return false
	}
	if !blankComposerRow(rows[cursor.Row]) {
		return cursor.Col == contentEndCol(rows[cursor.Row])
	}
	insert, ok := composerInsertCol(rows)
	return ok && cursor.Col == insert
}

// composerInsertCol measures the column where composer text begins: the
// smallest first-content column across the block's content rows (rows of pure
// decoration contribute nothing). For "› text" or its "  continuation" rows
// that is 2; for a boxed "│ › x │" row, 4.
func composerInsertCol(rows []string) (int, bool) {
	first, end, ok := composerBlockBounds(rows)
	if !ok {
		return 0, false
	}
	insert := -1
	for j := first; j < end; j++ {
		if col := firstContentCol(rows[j]); col >= 0 && (insert < 0 || col < insert) {
			insert = col
		}
	}
	return insert, insert >= 0
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
	if !ok {
		return observation
	}
	graceText := normalizeDelivery(pane)
	unverified := deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	if !stagedInComposer(pane, cursor, probe, bound, 1) {
		// The evidence no longer anchors the draft's insertion point. If the
		// live composer still contains it, the draft never submitted — it is
		// stranded where the grace frame found it, now possibly merged with
		// text that is not ours (a user typing moves the cursor off the tail
		// and drops the anchor without removing the draft). The boundary bound
		// this prompt, so a readable frame that still shows it must not round
		// up to the earlier landed observation (#4530 review).
		if composerHoldsBoundEvidence(pane, probe, bound) {
			log.WarningLog.Printf("submit: session %q still shows this prompt's bound evidence inside the composer after Enter, "+
				"but it no longer sits at the draft's insertion point, so the draft is stranded and possibly merged with other input; "+
				"withholding the remedy Enter and reporting sent-unverified (#4200). Pane tail: %s",
				t.sanitizedName, oneLineTail(pane))
			return unverified
		}
		return observation
	}
	if !frameStill(boundaryText, graceText) {
		log.WarningLog.Printf("submit: session %q shows this prompt staged in the composer after Enter, but the pane changed since the Enter, "+
			"so the composer may no longer hold only this prompt; withholding the remedy Enter and reporting sent-unverified (#4200). Pane tail: %s",
			t.sanitizedName, oneLineTail(pane))
		return unverified
	}

	// Stillness is measured on NORMALIZED text, which erases whitespace: a user
	// typing a space or a blank line into a continuation row during the grace
	// changes the draft without changing the comparison. The cursor betrays it
	// — typed input always moves it off the column the absorbed Enter left it
	// at. Purity is the same property one level up: the composer block must
	// render this payload and nothing else, or a paste the pre-boundary window
	// interleaved submits along with our draft.
	if !composerCursorIsHome(pane, cursor) {
		log.WarningLog.Printf("submit: session %q still shows this prompt staged, but the composer cursor no longer rests where "+
			"an undisturbed draft would leave it — input normalizeDelivery cannot see has moved it; "+
			"withholding the remedy Enter and reporting sent-unverified (#4200)", t.sanitizedName)
		return unverified
	}
	if !composerRenderPure(pane, probe, bound) {
		log.WarningLog.Printf("submit: session %q still shows this prompt staged, but the composer also holds text that is not this "+
			"delivery's render, so the remedy Enter could submit more than our draft; withholding it and reporting sent-unverified (#4200)",
			t.sanitizedName)
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
		stagedInComposer(settled, settledCursor, probe, bound, 2) ||
		composerHoldsBoundEvidence(settled, probe, bound) {
		return unverified
	}
	if observation.outcome == deliveryObservedLanded {
		return deliveryObservation{outcome: deliveryObservedLanded, pane: settled}
	}
	return deliveryObservation{outcome: deliveryObservedUnverified, pane: settled}
}
