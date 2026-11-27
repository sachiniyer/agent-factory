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
// counted there and cannot bind. boundaryText is the -J (joined) capture for
// text counts; boundaryGrid is the same instant in physical grid rows and is
// where position is judged. The count must GROW or the evidence must sit in
// the live composer, never merely be present on the pane.
func (p deliveryProbe) boundEvidence(boundaryText, boundaryGrid string) stagedEvidence {
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
	if bound.tail && bound.chip {
		return bound
	}
	// Positional binding: on a history-less pane an older identical copy can
	// scroll off the top exactly as this render lands, holding every count
	// flat (#4884's shape, applied to binding — #4530 review). What count
	// cannot prove, position still can: the live composer is the rows at and
	// below the boundary's last composer-glyph row, and a scrolled-off echo
	// sits above it, so evidence inside that region is this delivery's.
	rows := paneGridRows(xansi.Strip(boundaryGrid))
	anchor := lastComposerGlyphRow(rows)
	if anchor < 0 {
		return bound
	}
	region := normalizeDelivery(strings.Join(composerRegion(rows, anchor), ""))
	if !bound.tail && strings.Contains(region, p.completion) {
		bound.tail = true
	}
	// A chip binds positionally only when ITS count also grew: a same-sized
	// chip that predates the paste (an uncleared user draft) must not alias
	// as ours even when it sits in the composer.
	if !bound.chip {
		if chip, ok := newestPasteChip(region); ok && p.chipMatchesPayload(chip) &&
			strings.Count(boundaryText, chip) > strings.Count(p.baselineText, chip) {
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
	target := ExactTarget(t.sanitizedName)
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

// busyComposerHints are the hints an agent draws inside its EMPTY input row
// while it takes input mid-turn, normalized and keyed by that agent's composer
// glyph. capture-pane -p carries no attributes, so a dim hint reads exactly
// like typed text. Without this table, a busy agent's emptied composer read as
// a repaint holding foreign content. That was the withheld-remedy warning on
// 252 of the 537 deliveries production logged in #7023.
//
// An entry matches only the whole row, and only under its own glyph. A row
// that adds, drops or changes a character is content, and so are the same
// words under another agent's glyph. Idle hints are left out, both Devin's
// ("Ask Devin to build features…") and Codex's, which it draws idle and busy
// alike. An idle hint is also what a composer that dropped the prompt would
// show (#4200), so it stays unattributable.
//
// This is not composer_draft.go's composerPlaceholders. That table answers
// "does the composer hold a draft that blocks an account handoff", and for
// that question a queued message is still input the agent has not run.
var busyComposerHints = map[string][]string{
	// Claude Code, a message queued behind a running turn.
	claudeComposerGlyph: {normalizeDelivery("Press up to edit queued messages")},
	// Devin mid-turn.
	"❭": {normalizeDelivery("Guide Devin while it works")},
}

// bareComposerGlyphRow reports whether a row is an empty composer prompt: the
// bare glyph a composer repaints after a submit, or that glyph with the
// agent's own mid-turn hint (busyComposerHints), and nothing else. A user's
// ">quote" row is not bare, because it carries content. Codex's Ultra-tier
// "»" is not in the set: the recheck's wider stagedComposerGlyphRow
// recognizes it as the live composer's prompt, but a bare "»" row alone is
// not proof of an empty composer.
func bareComposerGlyphRow(row string) bool {
	norm := normalizeDelivery(row)
	for _, glyph := range []string{"›", claudeComposerGlyph, "❭"} {
		rest, ok := strings.CutPrefix(norm, glyph)
		if !ok {
			continue
		}
		if rest == "" {
			return true
		}
		for _, hint := range busyComposerHints[glyph] {
			if rest == hint {
				return true
			}
		}
		return false
	}
	return false
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

// boundaryBlock is where the Enter-boundary frame proved this delivery's
// composer block sat. anchor is that frame's last composer-glyph row over the
// recheck's (wide) glyph set — the row the staged reading compares movement
// against. blockStart is the composer block's FIRST row — the earliest row
// the bound draft's render could occupy — or -1 when the frame cannot
// establish it.
type boundaryBlock struct {
	anchor     int
	blockStart int
}

// deliveryBoundaryBlock reads the Enter boundary's composer block. The
// boundary capture runs in the same tmux command queue as our Enter, so it is
// the one frame where the bound draft provably sat: the block's first row is
// the row where the bound render's lowest on-pane occurrence begins — the
// whole normalized payload for a literal draft, the bound chip's own text
// for a collapsed paste. Glyph rows alone cannot find it: a draft
// continuation line can lead with ANY composer glyph — or be blank — so the
// deepest glyph-leading row can sit INSIDE the draft and leave its head
// above the anchor unread (#7809). When the bound render is not verbatim in
// the frame — a head scrolled off a history-less pane, a transformed render —
// the block start is unprovable and stays -1.
func deliveryBoundaryBlock(boundaryGrid string, probe deliveryProbe, bound stagedEvidence) boundaryBlock {
	rows := paneGridRows(boundaryGrid)
	b := boundaryBlock{anchor: stagedLastComposerAnchor(rows), blockStart: -1}
	if b.anchor < 0 {
		return b
	}
	end := b.anchor + len(composerRegion(rows, b.anchor))
	if bound.tail {
		b.blockStart = boundDraftHeadRow(rows, b.anchor, end, probe.payload)
	}
	if bound.chip {
		if r := boundDraftHeadRow(rows, b.anchor, end, bound.chipText); r >= 0 && (b.blockStart < 0 || r < b.blockStart) {
			b.blockStart = r
		}
	}
	return b
}

// boundDraftHeadRow is the row where the bound render's lowest occurrence
// begins: the first row, walking up from the boundary's anchor, whose rows
// through the composer block's bottom still cover the whole normalized
// render. Anything shallower starts the render mid-block — the draft's own
// glyph-leading or blank continuation rows can sit between its head and the
// anchor, so the head is found by content, never by glyph shape.
func boundDraftHeadRow(rows []string, anchor, end int, text string) int {
	if text == "" {
		return -1
	}
	for r := anchor; r >= 0; r-- {
		if strings.Contains(normalizeDelivery(strings.Join(rows[r:end], "")), text) {
			return r
		}
	}
	return -1
}

// boundStillStaged reports whether evidence bound to this delivery still sits
// in the live composer — at or below the composer block's first row, the
// earliest row the bound draft could occupy — in an atomic pane+cursor frame.
// The anchor is positional, not textual: a composer the clear+paste could not
// fully attribute must not read a transcript echo (which carries the same
// tail, and the same chip, above the live composer).
//
// When the grace frame's glyph row moved off the boundary's, the composer was
// repainted. A bare fresh glyph row BELOW the old anchor is a submitted
// composer; anything else — an anchor that moved up, or a content row like a
// user's ">quote" — is a composer shape we can no longer attribute, so the
// draft conservatively still counts as staged.
func boundStillStaged(pane string, probe deliveryProbe, bound stagedEvidence, boundary boundaryBlock) bool {
	return stagedReading(pane, probe, bound, boundary) != ""
}

// stagedReading is boundStillStaged's verdict with its reason. It names
// which of the staged readings fired, or returns "" when the draft left the
// live composer. The withheld-remedy warning prints the reason. A draft our
// Enter left in the composer and a repaint we cannot attribute are different
// failures, and a log line that named neither could not tell production's 537
// warnings apart (#7023).
func stagedReading(pane string, probe deliveryProbe, bound stagedEvidence, boundary boundaryBlock) string {
	rows := paneGridRows(pane)
	// The anchor uses the recheck's wider glyph set
	// (stagedLastComposerAnchor): Codex's Ultra tier prompts its composer
	// with "»" — the only line-leading "»" the TUI draws
	// (bottom_pane/effort_ignition.rs EffortTier::prompt_glyph) — while every
	// echoed user prompt is marked "› " at every tier
	// (history_cell/messages.rs). Under the narrower set an Ultra pane's
	// last "›" echo row won the anchor and the echo's own text read as this
	// prompt still staged — the literal-tail warnings that dominate #7801.
	anchor := stagedLastComposerAnchor(rows)
	if anchor < 0 {
		return ""
	}
	// The composer block's first row is the EARLIEST row this delivery's
	// draft could occupy, never the deepest glyph-leading row: a draft's own
	// continuation line can lead with ANY composer glyph — a lone "›" or ">"
	// inside a "›"/"❯"/"❭" composer — or be blank, so either glyph anchor
	// can land INSIDE the draft and leave its head above the search unread
	// (#7802 R1, #7809). The boundary frame proved where the block sat, so
	// this frame's block start is the boundary's own first row translated by
	// the anchor's movement; the narrow anchor stays as the earlier floor —
	// searching a transcript row above the block can only add staged
	// readings. When the boundary could not establish the block the search
	// runs from the pane's first row, the earliest candidate: a transcript
	// echo of the bound text then reads staged, which is safe.
	blockStart := lastComposerGlyphRow(rows)
	if blockStart < 0 {
		blockStart = anchor
	}
	if boundary.blockStart < 0 {
		blockStart = 0
	} else {
		head := boundary.blockStart + anchor - boundary.anchor
		if head < 0 {
			head = 0
		}
		if head < blockStart {
			blockStart = head
		}
	}
	lower := normalizeDelivery(strings.Join(rows[blockStart:], ""))
	if bound.tail && strings.Contains(lower, probe.completion) {
		return "this prompt's bound text is still in the live composer"
	}
	if bound.chip && strings.Contains(lower, bound.chipText) {
		return "this prompt's bound chip is still in the live composer"
	}
	if anchor == boundary.anchor {
		return ""
	}
	if anchor < boundary.anchor {
		return "the composer was repainted above its row at the Enter boundary, so what it holds cannot be attributed"
	}
	if !bareComposerGlyphRow(rows[anchor]) {
		if idleComposerPlaceholderRow(normalizeDelivery(rows[anchor])) {
			return "the composer was repainted holding only the agent's idle placeholder, which is exactly what a dropped prompt shows"
		}
		return "the composer was repainted holding content that is neither this prompt nor an empty composer"
	}
	return ""
}

// idleComposerPlaceholders are the ghost texts an agent draws inside an EMPTY
// input while it is not working a turn — normalized and keyed by the agent's
// composer glyph, the same discipline busyComposerHints keeps. They are
// exactly what a composer that dropped the prompt also shows (#4200), so a
// placeholder alone can never be positive delivery evidence — it only names
// the staged reading what it saw. Codex draws "Ask Codex to do anything"
// idle and busy alike, under both of its prompt glyphs. Claude rotates its
// `Try "…"` suggestions, so it is handled by shape below rather than listed.
var idleComposerPlaceholders = map[string][]string{
	"›": {normalizeDelivery("Ask Codex to do anything")},
	"»": {normalizeDelivery("Ask Codex to do anything")},
	"❭": {normalizeDelivery("Ask Devin to build features, fix bugs, or work on your code")},
}

// idleComposerPlaceholderRow reports whether a NORMALIZED composer row is the
// agent's glyph plus exactly its own idle placeholder — no more, no less,
// like busyComposerHints. The same text under another agent's glyph is
// content, and so is one added or dropped character.
func idleComposerPlaceholderRow(norm string) bool {
	for glyph, placeholders := range idleComposerPlaceholders {
		rest, ok := strings.CutPrefix(norm, glyph)
		if !ok {
			continue
		}
		for _, placeholder := range placeholders {
			if rest == placeholder {
				return true
			}
		}
		return false
	}
	// Claude's `Try "…"` hint rotates, so only the shape is provable — and
	// only under Claude's own glyph (composer_draft.go's shaped rule).
	if rest, ok := strings.CutPrefix(norm, claudeComposerGlyph); ok {
		return strings.HasPrefix(rest, `Try"`)
	}
	return false
}

// stillChipComposer is the stillness+position proof for the one remediable
// shape: the boundary's composer region held the bound chip and nothing else,
// and the grace frame's composer region is byte-identical, plus at most the
// one whitespace-only row an absorbed Enter can append. Anything else stands
// down: a literal draft (tail-bound) never reaches here, a second chip or user
// text changes the region's joined content, a moved anchor means a repainted
// composer, and a footer redrawn inside the region changes the bytes. The
// visible cursor must rest where undisturbed input left it: on the chip row at
// its insert or content-end column — the chip's last wrapped physical row when
// its text wraps across grid rows — or on the absorbed Enter's blank row at the
// insert column. A hidden cursor (Claude's ordinary composer draws one) cannot
// prove that, so it withholds.
//
// The wrapped-tail column is accepted only when the chip STARTS on the anchor
// row: the anchor's content after its single prompt glyph must be a non-empty
// prefix of the chip, and each later non-blank region row must continue it. A
// bare-glyph anchor row (a lone ›, >, ❭ or ❯ with nothing after it) is a hard
// newline in a user's draft, not the chip's first row — the region's joined
// content still matches the chip once that lone glyph is stripped as the
// leading prompt glyph, so the wrapped-tail acceptance must not fire, or a
// wrong accept would submit the user's own draft together with our prompt.
//
// The second return names the condition that failed, so the withheld-remedy
// warning says WHICH of hidden cursor, foreign composer content, moved
// anchor, changed region or misplaced cursor made the shape unprovable
// (#6224): a bare "not a still, single-chip composer" cannot tell a Claude
// pane (hidden cursor, by design) from a Codex pane whose footer bled into
// the region.
func stillChipComposer(boundary, pane string, cursor paneCursorState, chipText string) (bool, string) {
	if !cursor.Visible {
		return false, "the cursor is hidden, so where the draft left it cannot be proven (Claude's ordinary composer hides it)"
	}
	bRows := paneGridRows(boundary)
	gRows := paneGridRows(pane)
	anchor := lastComposerGlyphRow(bRows)
	if anchor < 0 || anchor >= len(gRows) || lastComposerGlyphRow(gRows) != anchor {
		return false, "the composer's prompt-glyph row moved or is missing between the Enter boundary and the grace frame"
	}
	bRegion := composerRegion(bRows, anchor)
	gRegion := composerRegion(gRows, anchor)
	// The boundary composer must hold the bound chip and nothing else. Compare
	// the region's joined content: the chip itself can wrap across physical
	// grid rows, and a row-count check would read its second half as foreign
	// content. Strip exactly one leading prompt glyph, not one of each — the
	// payload itself may open with '>'.
	regionContent := normalizeDelivery(strings.Join(bRegion, ""))
	if r, size := utf8.DecodeRuneInString(regionContent); r == '❯' || r == '›' || r == '❭' || r == '>' {
		regionContent = regionContent[size:]
	}
	if regionContent != chipText {
		return false, fmt.Sprintf("the composer region at the Enter boundary held %q, not only the bound chip %q", regionContent, chipText)
	}
	// Stillness: every boundary region row must appear, in order, in the grace
	// region; the grace region may add at most one row, and only whitespace.
	if len(gRegion) > len(bRegion)+1 {
		return false, "the composer region grew by more than the one blank row an absorbed Enter appends"
	}
	i := 0
	for _, row := range gRegion {
		if i < len(bRegion) && row == bRegion[i] {
			i++
			continue
		}
		if !blankComposerRow(row) {
			return false, "a composer row changed between the Enter boundary and the grace frame"
		}
	}
	if i != len(bRegion) {
		return false, "a composer row present at the Enter boundary is gone from the grace frame"
	}
	// Position: the cursor must sit inside the region, parked where this draft
	// alone could have left it.
	if cursor.Row < anchor || cursor.Row >= anchor+len(gRegion) || cursor.Row >= len(gRows) {
		return false, "the cursor is outside the composer region"
	}
	insert := firstContentCol(gRows[anchor])
	if blankComposerRow(gRows[cursor.Row]) {
		if insert >= 0 && cursor.Col == insert {
			return true, ""
		}
		return false, "the cursor is on a blank composer row but not at the insert column"
	}
	if cursor.Row == anchor && (cursor.Col == insert || cursor.Col == contentEndCol(gRows[anchor])) {
		return true, ""
	}
	// The chip can wrap across physical grid rows: an undisturbed draft parks
	// the cursor just past its tail on the chip's LAST wrapped row, not on the
	// anchor (glyph) row. The stillness proof above already bounds the region
	// to the chip's bytes (plus at most one absorbed-Enter blank row), so
	// accept the content-end column on the cursor's row when every region row
	// after it is blank — i.e. the cursor row is the chip's tail. A middle
	// wrapped row has non-blank content after it and is not where an
	// undisturbed draft leaves the cursor. This is only safe when the chip
	// STARTS on the anchor row: a bare-glyph anchor (a hard newline in a
	// user's draft) would otherwise let the wrapped tail of a chip on a later
	// row read as still, submitting the user's draft with our prompt.
	if !chipStartsOnAnchorRow(gRegion, chipText) {
		return false, "the chip does not start on the composer's anchor row, so a wrapped tail below it cannot be proven still"
	}
	blankTail := true
	for j := cursor.Row + 1; j < anchor+len(gRegion); j++ {
		if !blankComposerRow(gRows[j]) {
			blankTail = false
			break
		}
	}
	if blankTail && cursor.Col == contentEndCol(gRows[cursor.Row]) {
		return true, ""
	}
	return false, "the cursor is not where an undisturbed draft leaves it"
}

// chipStartsOnAnchorRow reports whether the chip's text begins on the
// composer's anchor (glyph) row and each later non-blank region row continues
// it — the precondition for accepting a wrapped chip's tail-row cursor. The
// anchor's content after its single prompt glyph must be a non-empty prefix of
// the chip, and every later non-blank row must keep the cumulative joined
// content a prefix of the chip. A bare-glyph anchor row (a lone ›, >, ❭ or ❯
// with nothing after it) is a hard newline in a user's draft: its glyph is
// stripped as the leading prompt glyph by the region-equality check, so the
// region still matches the chip even though the chip starts on a later row —
// exactly the shape the wrapped-tail acceptance must not fire on. A blank row
// is tolerated only AFTER the chip is fully matched (the absorbed-Enter
// continuation row); a blank row interleaved between the chip's wrapped rows
// is an interior break in the chip and must fail the proof, or a changed
// composer could pass a wrapped tail that is not the undisturbed draft.
func chipStartsOnAnchorRow(region []string, chipText string) bool {
	if len(region) == 0 {
		return false
	}
	acc := normalizeDelivery(region[0])
	if r, size := utf8.DecodeRuneInString(acc); r == '❯' || r == '›' || r == '❭' || r == '>' {
		acc = acc[size:]
	}
	if acc == "" || !strings.HasPrefix(chipText, acc) {
		return false
	}
	for j := 1; j < len(region); j++ {
		if blankComposerRow(region[j]) {
			if acc == chipText {
				continue
			}
			return false
		}
		acc += normalizeDelivery(region[j])
		if !strings.HasPrefix(chipText, acc) {
			return false
		}
	}
	return true
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
//  3. Position. The bound evidence sits in the live composer — at or below
//     the composer block's first row, the earliest row the bound draft could
//     occupy, derived from where the boundary frame proves the draft sat
//     (boundStillStaged) — and the cursor is visible and parked where this
//     draft alone could have left it. A transcript echo cannot satisfy
//     position: the composer repaints its own glyph row beneath the echo.
//
// What the conditions do not cover: the gap between the grace capture and the
// remedy keystroke is check-then-act, like absenceStillProven's. The daemon's
// attach defer (#1586) is the guard for a user typing in a pane.
//
// The verdict never rounds up. Bound evidence that stays staged, a pane that
// changed while staged, a remedy whose outcome cannot be re-read, and every
// non-chip staged shape all report sent-unverified. Only a draft that left the
// composer keeps the observation it had before Enter.
func (t *TmuxSession) remedyStrandedSubmit(probe deliveryProbe, observation deliveryObservation, bound stagedEvidence, boundaryGrid string, boundaryOK bool) deliveryObservation {
	if !boundaryOK || !bound.any() {
		return observation
	}
	stripped := xansi.Strip(boundaryGrid)
	boundary := deliveryBoundaryBlock(stripped, probe, bound)

	pasteDeliverySleep(strandedSubmitGrace)
	pane, cursor, ok := t.capturePaneAndCursorState()
	if !ok {
		return observation
	}
	unverified := deliveryObservation{outcome: deliveryObservedUnverified, pane: pane}
	reading := stagedReading(pane, probe, bound, boundary)
	if reading == "" {
		return observation
	}

	// The draft is still staged. Only the bound chip in a byte-still composer
	// authorizes the remedy Enter; every other staged shape — a literal draft,
	// foreign content, a moved or hidden cursor — stands down.
	why := "the bound evidence is a literal tail, not a collapsed-paste chip"
	stillChip := false
	if bound.chip {
		stillChip, why = stillChipComposer(stripped, pane, cursor, bound.chipText)
	}
	if !stillChip {
		log.WarningLog.Printf("submit: session %q reads as still staged after Enter (%s), "+
			"but it is not a still, single-chip composer this delivery can prove is only ours (%s); "+
			"withholding the remedy Enter and reporting sent-unverified (#4200). Pane tail: %s",
			t.sanitizedName, reading, why, oneLineTail(pane))
		return unverified
	}

	log.WarningLog.Printf("submit: session %q still holds this prompt's bound chip staged in a byte-identical composer after Enter; "+
		"the keystroke was absorbed, so sending one more Enter to submit the same draft (#4200)", t.sanitizedName)
	remedyBoundary, _, remedyBoundaryOK, err := t.sendEnterAndCaptureBoundary()
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
	if !ok || boundStillStaged(settled, probe, bound, boundary) {
		return unverified
	}
	outcome := observation.outcome
	if outcome == deliveryObservedAbsent {
		// A bound draft that left the composer was whole at Enter and has now
		// submitted: the absent classification was stale pre-Enter evidence, and
		// returning it would hand the redelivery path a proven-false absence.
		outcome = deliveryObservedUnverified
	}
	return deliveryObservation{outcome: outcome, pane: settled}
}
