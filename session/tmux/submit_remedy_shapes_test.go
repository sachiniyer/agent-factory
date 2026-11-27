package tmux

import (
	"strings"
	"testing"
)

// Pane shapes copied from real captures (af sessions preview of a Claude lane in
// vim INSERT mode, and a Codex lane with its footer), with a collapsed-paste chip
// in the composer. #6224 asked whether these ordinary shapes defeat the bound-
// evidence check: the composer's status line (-- INSERT --, the footer and its
// warning chip) must never be read as staged prompt text, and the reason a
// shape is withheld must be named.

const (
	claudeChip = "[Pasted text #1 +10 lines]"
	codexChip  = "[Pasted Content 3207 chars]"
)

// chipBlockForTest derives the boundaryBlock remedyStrandedSubmit computes
// for a fixture whose only bound evidence is this delivery's chip.
func chipBlockForTest(boundary, chipText string) boundaryBlock {
	return deliveryBoundaryBlock(boundary, deliveryProbe{}, stagedEvidence{chip: true, chipText: chipText})
}

func claudeInsertPane(composer string) string {
	return strings.Join([]string{
		"✻ Cogitated for 4m 47s · done 3:53 AM",
		"                                        ✔ Update installed · /restart to apply",
		"────────────────────────────────────────────────────────────────────────────────",
		composer,
		"────────────────────────────────────────────────────────────────────────────────",
		"",
		"  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent",
	}, "\n")
}

func codexWarningPane(composer string, blankBeforeFooter bool) string {
	rows := []string{
		"  Worked for 1m 32s — 02:01",
		"",
		composer,
	}
	if blankBeforeFooter {
		rows = append(rows, "")
	}
	rows = append(rows, "  GPT-6.1 Terra ultra · ~/sahara/sahara-cheaper-ci · ⚠ 1 warning · f2 to view")
	return strings.Join(rows, "\n")
}

func TestComposerRegionStopsBeforeTheStatusLine(t *testing.T) {
	for name, pane := range map[string]string{
		"claude INSERT": claudeInsertPane("❯ " + claudeChip),
		"codex warning": codexWarningPane("› "+codexChip, true),
	} {
		rows := paneGridRows(pane)
		anchor := lastComposerGlyphRow(rows)
		if anchor < 0 {
			t.Fatalf("%s: no composer glyph row found", name)
		}
		region := composerRegion(rows, anchor)
		for _, row := range region {
			if strings.Contains(row, "INSERT") || strings.Contains(row, "warning") || strings.Contains(row, "bypass") {
				t.Errorf("%s: the status line leaked into the composer region: %q", name, row)
			}
		}
	}
}

func TestChipComposerVerdictNamesWhyAShapeIsWithheld(t *testing.T) {
	claude := claudeInsertPane("❯ " + claudeChip)
	chipText := normalizeDelivery(claudeChip)

	// Claude's ordinary composer hides its cursor: unprovable by design, and the
	// reason says so rather than leaving a generic "not a still, single-chip composer".
	ok, why := stillChipComposer(claude, chipBlockForTest(claude, chipText), claude, paneCursorState{Visible: false}, chipText)
	if ok || !strings.Contains(why, "cursor is hidden") {
		t.Fatalf("hidden cursor: ok=%v why=%q, want a withheld verdict naming the hidden cursor", ok, why)
	}
	// With a visible cursor parked after the chip the very same pane IS provable: the
	// INSERT line below the composer is not read as foreign composer content.
	row := "❯ " + claudeChip
	ok, why = stillChipComposer(claude, chipBlockForTest(claude, chipText), claude, paneCursorState{Visible: true, Row: 3, Col: len([]rune(row))}, chipText)
	if !ok {
		t.Fatalf("visible cursor on the chip row: withheld (%s); the INSERT status line must not defeat the check", why)
	}

	// Codex with its warning footer a blank row below the composer is provable...
	codex := codexWarningPane("› "+codexChip, true)
	codexChipText := normalizeDelivery(codexChip)
	codexRow := "› " + codexChip
	ok, why = stillChipComposer(codex, chipBlockForTest(codex, codexChipText), codex, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, codexChipText)
	if !ok {
		t.Fatalf("codex composer with a warning footer: withheld (%s)", why)
	}
	// ...while a footer drawn with no blank row between it and the composer is
	// composer content as far as the region goes, and the reason names that.
	tight := codexWarningPane("› "+codexChip, false)
	ok, why = stillChipComposer(tight, chipBlockForTest(tight, codexChipText), tight, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, codexChipText)
	if ok || !strings.Contains(why, "not only the bound chip") {
		t.Fatalf("footer without a gap: ok=%v why=%q, want it named as foreign composer content", ok, why)
	}
}

// A submitted composer repaints a bare glyph row; the status line below it must
// not make the chip read as still staged.
func TestBoundChipLeavingTheComposerIsNotStillStagedBelowAStatusLine(t *testing.T) {
	bound := stagedEvidence{chip: true, chipText: normalizeDelivery(claudeChip)}
	staged := claudeInsertPane("❯ " + claudeChip)
	submitted := claudeInsertPane("❯")
	block := deliveryBoundaryBlock(staged, deliveryProbe{}, bound)
	if !boundStillStaged(staged, deliveryProbe{}, bound, block) {
		t.Fatal("a chip still in the composer must read as staged")
	}
	if boundStillStaged(submitted, deliveryProbe{}, bound, block) {
		t.Fatal("a bare repainted composer above an INSERT status line must read as submitted")
	}
}

// TestStillChipComposerAcceptsWrappedChipTailCursor pins the wrap-aware cursor
// position proof (#4200 wrapped-chip fix): a paste chip long enough to wrap
// across physical grid rows parks the cursor just past its tail on the chip's
// LAST wrapped row, not on the anchor (glyph) row. The stillness proof already
// bounds the region to exactly the chip's bytes, so that content-end position
// must be accepted — otherwise a provably still, bound, wrapped-chip strand is
// incorrectly withheld with "the cursor is not where an undisturbed draft
// leaves it" on the very row an undisturbed draft leaves it.
func TestStillChipComposerAcceptsWrappedChipTailCursor(t *testing.T) {
	chipText := normalizeDelivery(codexChip)

	// A 20-column pane wraps "› [Pasted Content 3207 chars]" (one logical
	// composer row) across two physical grid rows; boundary and grace frames
	// are byte-identical, and the cursor rests at the content end of the
	// chip's last wrapped row "07 chars]".
	twoRow := strings.Join([]string{
		"  Worked for 1m 32s — 02:01",
		"",
		"› [Pasted Content 32",
		"07 chars]",
	}, "\n") + "\n"
	tail := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("07 chars]")}
	if ok, why := stillChipComposer(twoRow, chipBlockForTest(twoRow, chipText), twoRow, tail, chipText); !ok {
		t.Fatalf("wrapped chip, cursor at the wrapped tail: ok=false why=%q, want ok=true (the cursor is exactly where an undisturbed draft leaves it for a wrapped chip)", why)
	}

	// A narrower pane wraps the same chip across three rows. The cursor at the
	// content end of the chip's last wrapped row is still accepted...
	threeRow := strings.Join([]string{
		"  Worked for 1m 32s — 02:01",
		"",
		"› [Pasted ",
		"Content 32",
		"07 chars]",
	}, "\n") + "\n"
	tail3 := paneCursorState{Visible: true, Row: 4, Col: contentEndCol("07 chars]")}
	if ok, why := stillChipComposer(threeRow, chipBlockForTest(threeRow, chipText), threeRow, tail3, chipText); !ok {
		t.Fatalf("wrapped chip (three rows), cursor at the wrapped tail: ok=false why=%q, want ok=true", why)
	}
	// ...while a cursor parked at the content end of a MIDDLE wrapped row is
	// rejected: an undisturbed draft never leaves the cursor mid-chip.
	middle := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("Content 32")}
	if ok, why := stillChipComposer(threeRow, chipBlockForTest(threeRow, chipText), threeRow, middle, chipText); ok {
		t.Fatalf("cursor on a middle wrapped row: ok=true why=%q, want ok=false (the cursor is mid-chip, not at the tail)", why)
	}

	// A cursor on the wrapped tail row but at the wrong column is rejected.
	wrongCol := paneCursorState{Visible: true, Row: 3, Col: 5}
	if ok, why := stillChipComposer(twoRow, chipBlockForTest(twoRow, chipText), twoRow, wrongCol, chipText); ok {
		t.Fatalf("wrapped chip, cursor at the wrong column on the tail row: ok=true why=%q, want ok=false", why)
	}

	// Control 1: the same chip on a single row with the cursor at the anchor
	// row's content end is still accepted — wrapping alone is what used to
	// defeat the check, not the resting position.
	oneRow := strings.Join([]string{
		"  Worked for 1m 32s — 02:01",
		"",
		"› " + codexChip,
	}, "\n") + "\n"
	endCol := contentEndCol("› " + codexChip)
	if ok, why := stillChipComposer(oneRow, chipBlockForTest(oneRow, chipText), oneRow, paneCursorState{Visible: true, Row: 2, Col: endCol}, chipText); !ok {
		t.Fatalf("unwrapped chip, cursor at content end: ok=false why=%q, want ok=true (no regression)", why)
	}

	// Control 2: a wrapped chip that absorbed Enter as a newline (one blank
	// continuation row) with the cursor on that blank row at the insert
	// column is still accepted — the canonical absorbed-newline strand
	// remedies even when the chip wraps.
	absorbed := strings.Join([]string{
		"  Worked for 1m 32s — 02:01",
		"",
		"› [Pasted Content 32",
		"07 chars]",
		"",
		"  GPT-6.1 Terra ultra · ~/sahara/sahara-cheaper-ci · ⚠ 1 warning · f2 to view",
	}, "\n") + "\n"
	insert := firstContentCol("› [Pasted Content 32")
	if ok, why := stillChipComposer(absorbed, chipBlockForTest(absorbed, chipText), absorbed, paneCursorState{Visible: true, Row: 4, Col: insert}, chipText); !ok {
		t.Fatalf("wrapped chip with absorbed Enter, cursor on the blank row at the insert column: ok=false why=%q, want ok=true (no regression)", why)
	}
}

// TestStillChipComposerWithholdsWrappedTailBelowABareGlyphAnchor pins the
// guard on the wrapped-tail acceptance: the tail-row cursor is accepted only
// when the chip STARTS on the composer's anchor row. A bare-glyph anchor row
// (a lone ›, >, ❭ or ❯ with nothing after it) is a hard newline in a user's
// draft, not the chip's first row — the region's joined content still matches
// the chip once that lone glyph is stripped as the leading prompt glyph, so
// without the guard the wrapped tail of a chip on a later row would read as
// still and the remedy Enter would submit the user's own draft together with
// our prompt. Each frame below is byte-still and region-equal to the chip, with
// the visible cursor at the content-end of the chip's (wrapped) tail row; the
// remedy must withhold on all of them.
func TestStillChipComposerWithholdsWrappedTailBelowABareGlyphAnchor(t *testing.T) {
	chipText := normalizeDelivery(codexChip)

	// Draft + lone ">" + chip (unwrapped): the user's draft ended with a hard
	// newline and a lone ">", then our chip landed on the next row. The last
	// composer-glyph row is the lone ">", so it is the anchor; the chip sits on
	// a later row. Cursor at the chip row's content end.
	draftLoneGtUnwrapped := strings.Join([]string{
		"› please also delete the",
		"  staging bucket first",
		"  >",
		"  " + codexChip,
	}, "\n") + "\n"
	tail := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("  " + codexChip)}
	if ok, why := stillChipComposer(draftLoneGtUnwrapped, chipBlockForTest(draftLoneGtUnwrapped, chipText), draftLoneGtUnwrapped, tail, chipText); ok {
		t.Fatalf("draft + lone > + unwrapped chip, cursor at the chip tail: ok=true why=%q, want ok=false (the lone > is a hard newline in the draft, not the chip's first row)", why)
	}

	// Draft + lone ">" + chip (wrapped): the same shape with the chip wrapping
	// across two physical rows. The cursor rests at the content end of the
	// chip's last wrapped row — exactly where an undisturbed draft leaves it for
	// a wrapped chip, which is precisely why this must be guarded.
	draftLoneGtWrapped := strings.Join([]string{
		"› please also delete the",
		"  staging bucket first",
		"  >",
		"  [Pasted Content 32",
		"07 chars]",
	}, "\n") + "\n"
	tailWrapped := paneCursorState{Visible: true, Row: 4, Col: contentEndCol("07 chars]")}
	if ok, why := stillChipComposer(draftLoneGtWrapped, chipBlockForTest(draftLoneGtWrapped, chipText), draftLoneGtWrapped, tailWrapped, chipText); ok {
		t.Fatalf("draft + lone > + wrapped chip, cursor at the wrapped tail: ok=true why=%q, want ok=false (the chip does not start on the anchor row)", why)
	}

	// A lone "›" anchor (Codex's own glyph) below a draft, then the chip on a
	// later row — the same misread under a different composer glyph.
	loneCodexGlyph := strings.Join([]string{
		"› please also delete the",
		"  staging bucket first",
		"  ›",
		"  " + codexChip,
	}, "\n") + "\n"
	tailCodex := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("  " + codexChip)}
	if ok, why := stillChipComposer(loneCodexGlyph, chipBlockForTest(loneCodexGlyph, chipText), loneCodexGlyph, tailCodex, chipText); ok {
		t.Fatalf("draft + lone › + chip, cursor at the chip tail: ok=true why=%q, want ok=false (a lone › anchor is a hard newline, not the chip's first row)", why)
	}

	// An empty first line: a bare-glyph composer (empty first line of the draft)
	// followed by the chip on the next row. The chip did not start on the
	// anchor row, so the wrapped tail must withhold even with no user text
	// above it.
	emptyFirstLine := strings.Join([]string{
		"  Worked for 1m 32s — 02:01",
		"",
		"›",
		"  " + codexChip,
	}, "\n") + "\n"
	tailEmpty := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("  " + codexChip)}
	if ok, why := stillChipComposer(emptyFirstLine, chipBlockForTest(emptyFirstLine, chipText), emptyFirstLine, tailEmpty, chipText); ok {
		t.Fatalf("empty first line + chip on the next row, cursor at the chip tail: ok=true why=%q, want ok=false (the chip does not start on the bare-glyph anchor row)", why)
	}
}

// TestStillChipComposerWithholdsDraftTextAboveTheAnchor is the #7850 R2 frame:
// the anchor lands on the draft's chip-bearing continuation row while its real
// first row — the user's own "ship it" — sits above it. The region's joined
// content is still just the chip, so master's proof accepts the shape and the
// remedy Enter submits the user's line with the prompt. A draft's own head, or
// a transcript echo af cannot tell from one, makes the anchor ambiguous: the
// composer is not provably chip-only, so the remedy must withhold.
func TestStillChipComposerWithholdsDraftTextAboveTheAnchor(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	pane := strings.Join([]string{
		"› ship it",
		"  > " + codexChip,
	}, "\n") + "\n"
	tail := paneCursorState{Visible: true, Row: 1, Col: contentEndCol("  > " + codexChip)}
	if ok, why := stillChipComposer(pane, chipBlockForTest(pane, chipText), pane, tail, chipText); ok {
		t.Fatalf("draft text above the anchor, cursor at the chip row's content end: ok=true why=%q, want ok=false (the anchor is not the composer block's first row)", why)
	}
}

// TestStillChipComposerWithholdsDraftTextBelowABlankRow is the #7850 R3 frame:
// a draft of "\n\nok go" renders the chip, a blank gap, then the user's line —
// textually the same shape composerRegion's footer cut was built for. master's
// proof cuts the region at "ok go", reads the chip, and the remedy Enter
// submits the user's line with the prompt. A content row after a blank gap is
// composer content unless it is provably the agent's own footer, and "ok go"
// is not, so the remedy must withhold.
func TestStillChipComposerWithholdsDraftTextBelowABlankRow(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	pane := strings.Join([]string{
		"› " + codexChip,
		"",
		"  ok go",
	}, "\n") + "\n"
	insert := firstContentCol("› " + codexChip)
	if ok, why := stillChipComposer(pane, chipBlockForTest(pane, chipText), pane, paneCursorState{Visible: true, Row: 0, Col: insert}, chipText); ok {
		t.Fatalf("draft text below a blank row, cursor at the draft's start: ok=true why=%q, want ok=false (\"ok go\" is not provably the agent's own footer)", why)
	}
}

// TestStillChipComposerAcceptsAgentFooterBelowABlankRow is the positive #7850
// R3 control: the same shape as a draft that opened with blank lines, but the
// row below the gap carries a signature the agent draws there — Codex's model/
// warning footer. The composer is provably chip-only and the remedy fires.
func TestStillChipComposerAcceptsAgentFooterBelowABlankRow(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	pane := strings.Join([]string{
		"› " + codexChip,
		"",
		"  GPT-6.1 Terra ultra · ~/sahara/sahara-cheaper-ci · ⚠ 1 warning · f2 to view",
	}, "\n") + "\n"
	end := contentEndCol("› " + codexChip)
	if ok, why := stillChipComposer(pane, chipBlockForTest(pane, chipText), pane, paneCursorState{Visible: true, Row: 0, Col: end}, chipText); !ok {
		t.Fatalf("codex warning footer below a blank row: ok=false why=%q, want ok=true (the footer is provably the agent's own)", why)
	}
}

// TestChipStartsOnAnchorRowRejectsAnInteriorBlank pins the wrapped-tail proof's
// blank handling: a blank row is tolerated only AFTER the chip is fully matched
// (the absorbed-Enter continuation row). A blank interleaved between the chip's
// wrapped rows is an interior break in the chip and must fail the proof —
// otherwise a composer that gained a blank row mid-chip could pass a wrapped
// tail that is not the undisturbed draft. (composerRegion never yields an
// interior blank — it cuts the region at the first content row after a blank —
// so this guards the helper directly against a future region shape.)
func TestChipStartsOnAnchorRowRejectsAnInteriorBlank(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	anchorRow := "› [Pasted Content 32"
	wrapRow := "07 chars]"

	// A trailing blank after the complete chip is the absorbed-Enter row: OK.
	if !chipStartsOnAnchorRow([]string{anchorRow, wrapRow, ""}, chipText) {
		t.Fatal("trailing blank after the complete chip should be accepted (the absorbed-Enter continuation row)")
	}
	// A blank interleaved between the chip's wrapped rows is an interior break.
	if chipStartsOnAnchorRow([]string{anchorRow, "", wrapRow}, chipText) {
		t.Fatal("interior blank between the chip's wrapped rows should be rejected (the chip is not a contiguous draft)")
	}
}
