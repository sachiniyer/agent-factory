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
	ok, why := stillChipComposer(claude, claude, paneCursorState{Visible: false}, chipText)
	if ok || !strings.Contains(why, "cursor is hidden") {
		t.Fatalf("hidden cursor: ok=%v why=%q, want a withheld verdict naming the hidden cursor", ok, why)
	}
	// With a visible cursor parked after the chip the very same pane IS provable: the
	// INSERT line below the composer is not read as foreign composer content.
	row := "❯ " + claudeChip
	ok, why = stillChipComposer(claude, claude, paneCursorState{Visible: true, Row: 3, Col: len([]rune(row))}, chipText)
	if !ok {
		t.Fatalf("visible cursor on the chip row: withheld (%s); the INSERT status line must not defeat the check", why)
	}

	// Codex with its warning footer a blank row below the composer is provable...
	codex := codexWarningPane("› "+codexChip, true)
	codexChipText := normalizeDelivery(codexChip)
	codexRow := "› " + codexChip
	ok, why = stillChipComposer(codex, codex, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, codexChipText)
	if !ok {
		t.Fatalf("codex composer with a warning footer: withheld (%s)", why)
	}
	// ...while a footer drawn with no blank row between it and the composer is
	// composer content as far as the region goes, and the reason names that.
	tight := codexWarningPane("› "+codexChip, false)
	ok, why = stillChipComposer(tight, tight, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, codexChipText)
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
	if ok, why := stillChipComposer(twoRow, twoRow, tail, chipText); !ok {
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
	if ok, why := stillChipComposer(threeRow, threeRow, tail3, chipText); !ok {
		t.Fatalf("wrapped chip (three rows), cursor at the wrapped tail: ok=false why=%q, want ok=true", why)
	}
	// ...while a cursor parked at the content end of a MIDDLE wrapped row is
	// rejected: an undisturbed draft never leaves the cursor mid-chip.
	middle := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("Content 32")}
	if ok, why := stillChipComposer(threeRow, threeRow, middle, chipText); ok {
		t.Fatalf("cursor on a middle wrapped row: ok=true why=%q, want ok=false (the cursor is mid-chip, not at the tail)", why)
	}

	// A cursor on the wrapped tail row but at the wrong column is rejected.
	wrongCol := paneCursorState{Visible: true, Row: 3, Col: 5}
	if ok, why := stillChipComposer(twoRow, twoRow, wrongCol, chipText); ok {
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
	if ok, why := stillChipComposer(oneRow, oneRow, paneCursorState{Visible: true, Row: 2, Col: endCol}, chipText); !ok {
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
	if ok, why := stillChipComposer(absorbed, absorbed, paneCursorState{Visible: true, Row: 4, Col: insert}, chipText); !ok {
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
	if ok, why := stillChipComposer(draftLoneGtUnwrapped, draftLoneGtUnwrapped, tail, chipText); ok {
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
	if ok, why := stillChipComposer(draftLoneGtWrapped, draftLoneGtWrapped, tailWrapped, chipText); ok {
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
	if ok, why := stillChipComposer(loneCodexGlyph, loneCodexGlyph, tailCodex, chipText); ok {
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
	if ok, why := stillChipComposer(emptyFirstLine, emptyFirstLine, tailEmpty, chipText); ok {
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
	if ok, why := stillChipComposer(pane, pane, tail, chipText); ok {
		t.Fatalf("draft text above the anchor, cursor at the chip row's content end: ok=true why=%q, want ok=false (the anchor is not the composer block's first row)", why)
	}
}

// TestStillChipComposerWithholdsDraftRowsAboveTheAnchor is the #7850 R1
// class: the anchor lands on the draft's chip-bearing continuation row while
// draft rows — plain text or blank — sit between it and the composer's top
// edge. The region's joined content is still just the chip, so every prior
// proof accepted the shape and the remedy Enter submitted the user's lines
// with the prompt. Each frame must withhold, on master's proof and on every
// proof that only checks glyph-led rows or an unreadable block start.
func TestStillChipComposerWithholdsDraftRowsAboveTheAnchor(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	for name, rows := range map[string][]string{
		// A2: a second draft line between the head and the chip row.
		"A2 draft lines above the chip row": {
			"› ship it",
			"  please",
			"  > " + codexChip,
		},
		// A2′: #7836's own rows — a wrapped draft head above the chip row.
		"A2′ wrapped draft head above the chip row": {
			"› please also delete the",
			"  staging bucket first",
			"  > " + codexChip,
		},
		// A4: a blank draft line between the head and the chip row — the
		// blank stops every glyph-row scan without being a top edge.
		"A4 blank draft row above the chip row": {
			"› ship it",
			"",
			"  > " + codexChip,
		},
		// A5b: a typed chip-shaped row above the real chip.
		"A5b typed chip text above the real chip": {
			"› ship it",
			"  " + codexChip,
			"  > " + codexChip,
		},
		// A6b: draft lines, a bare "›" line, then the chip row.
		"A6b draft and a bare › above the chip row": {
			"› ship it",
			"  please",
			"  ›",
			"  > " + codexChip,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pane := strings.Join(rows, "\n") + "\n"
			anchor := len(rows) - 1
			tail := paneCursorState{Visible: true, Row: anchor, Col: contentEndCol(rows[anchor])}
			if ok, why := stillChipComposer(pane, pane, tail, chipText); ok {
				t.Fatalf("ok=true why=%q, want ok=false (the anchor is not the composer block's first row)", why)
			}
		})
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
	if ok, why := stillChipComposer(pane, pane, paneCursorState{Visible: true, Row: 0, Col: insert}, chipText); ok {
		t.Fatalf("draft text below a blank row, cursor at the draft's start: ok=true why=%q, want ok=false (\"ok go\" is not provably the agent's own footer)", why)
	}
}

// TestStillChipComposerWithholdsDraftRowsBelowABlankRow is the #7850 R2/N1
// class: a ruled or boxed composer takes only blank rows between the
// region's cut and its own closing edge, and Codex's edgeless lane takes
// only rows that parse its closed footer token grammar — a draft line that
// merely contains a footer phrase, a digit-led word, a path-shaped prefix,
// or a bracketed clause is composer content, and a footer-shaped row never
// excuses the rows below it.
func TestStillChipComposerWithholdsDraftRowsBelowABlankRow(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	for name, tc := range map[string]struct {
		rows   []string
		cursor paneCursorState
	}{
		// B3: a draft line carrying a real marker phrase as substring.
		"B3 draft containing 'for agents'": {
			rows:   []string{"› " + codexChip, "", "  then update the docs for agents"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// B3b: a footer-looking row never excuses the draft rows below it.
		"B3b footer-shaped first row, draft below": {
			rows: []string{
				"› " + codexChip, "",
				"  esc to interrupt if this is slow",
				"  then run make deploy-prod",
				"  and delete the staging bucket",
			},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// B6: a draft line in the "text · ~/path" shape is not the Codex
		// status line — the model segment carries no known model token.
		"B6 draft in model/path shape": {
			rows:   []string{"› " + codexChip, "", "  see my notes · ~/notes.md", "  then force-push release"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// F14: a digit-led first word is not a model token, and a path
		// segment with spaces is not a whole path token.
		"F14 numbered draft in model/path shape": {
			rows:   []string{"› " + codexChip, "", "  1. see my notes · ~/notes.md then force-push release"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// F2: same hole through the path-shaped second segment.
		"F2 numbered draft with path tail": {
			rows:   []string{"› " + codexChip, "", "  1. force-push release · /srv/app"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// F3: "v2" is not a model token.
		"F3 v2 draft in model/path shape": {
			rows:   []string{"› " + codexChip, "", "  v2 · ~/notes.md then force-push release"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// F4: a Main […] bracket never holds a clause.
		"F4 Main bracket holding a clause": {
			rows:   []string{"› " + codexChip, "", "  Main [force-push release, then delete the staging bucket]"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// B5: an indented row of box-drawing glyphs is draft text, not the
		// composer's edge — a border ends the composer only at the agent's
		// own column-0 rule or box geometry.
		"B5 indented box-drawing row": {
			rows:   []string{"› " + codexChip, "", "  ──────────────"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// The literal round-1 B5: the indented dashes row and the draft row
		// below it, with no blank gap.
		"B5 literal indented dashes then draft": {
			rows:   []string{"› " + codexChip, "  ──────────────", "  ok go"},
			cursor: paneCursorState{Visible: true, Row: 0, Col: firstContentCol("› " + codexChip)},
		},
		// F15: a digit-led draft line inside Devin's rules — #7850's own B1
		// plus "2.". Only blank rows may sit between the region's cut and
		// the agent's closing rule.
		"F15 numbered draft inside Devin's rules": {
			rows: []string{
				strings.Repeat("─", 80),
				"❭ " + codexChip,
				"",
				"  2. ok go",
				strings.Repeat("─", 80),
			},
			cursor: paneCursorState{Visible: true, Row: 1, Col: firstContentCol("❭ " + codexChip)},
		},
		// F6: same shape, "then force-push" tail.
		"F6 numbered force-push inside Devin's rules": {
			rows: []string{
				strings.Repeat("─", 80),
				"❭ " + codexChip,
				"",
				"  2. then force-push release",
				strings.Repeat("─", 80),
			},
			cursor: paneCursorState{Visible: true, Row: 1, Col: firstContentCol("❭ " + codexChip)},
		},
		// F7: a "3 more things:" draft.
		"F7 numbered list draft inside Devin's rules": {
			rows: []string{
				strings.Repeat("─", 80),
				"❭ " + codexChip,
				"",
				"  3 more things: fix the build",
				strings.Repeat("─", 80),
			},
			cursor: paneCursorState{Visible: true, Row: 1, Col: firstContentCol("❭ " + codexChip)},
		},
		// F16: a "v2"-led draft carrying Devin's own mode word.
		"F16 v2 draft with mode word inside Devin's rules": {
			rows: []string{
				strings.Repeat("─", 80),
				"❭ " + codexChip,
				"",
				"  v2 ok go then force-push · bypass",
				strings.Repeat("─", 80),
			},
			cursor: paneCursorState{Visible: true, Row: 1, Col: firstContentCol("❭ " + codexChip)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pane := strings.Join(tc.rows, "\n") + "\n"
			if ok, why := stillChipComposer(pane, pane, tc.cursor, chipText); ok {
				t.Fatalf("ok=true why=%q, want ok=false (the rows below the blank gap are not the agent's own footer)", why)
			}
		})
	}
}

// TestStillChipComposerWithholdsAForeignClosingEdge is the #7912 item-2
// class: the closing edge is the composer's OWN bottom edge — byte-identical
// to the top edge above the anchor — never any column-0 all-"─" row or
// ╰───╯-shaped row of the wrong width. Under the column-0 wrap model the
// cursor proof accepts, a wrapped draft's continuation can draw an all-"─"
// row at column 0 of any width, and on master it counted as "a hard border"
// that excused every row beneath it. Every frame below remedies on master
// for exactly that reason and must withhold here.
func TestStillChipComposerWithholdsAForeignClosingEdge(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	rule := strings.Repeat("─", 80)
	devinChip := "❭ " + codexChip
	claudeChipRow := "❯ " + claudeChip
	for name, tc := range map[string]struct {
		rows   []string
		cursor paneCursorState
		chip   string
	}{
		// X1h: a single "─" at column 0 is not the composer's bottom rule.
		"X1h single dash closing edge": {
			rows:   []string{"one", rule, devinChip, "", "─", "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol(devinChip)},
			chip:   chipText,
		},
		// X1d: a wrapped draft's column-0 continuation narrower than the
		// composer's own rule.
		"X1d wrap-width rule below the cut": {
			rows:   []string{"one", rule, devinChip, "", strings.Repeat("─", 40), "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol(devinChip)},
			chip:   chipText,
		},
		// X1e: wider than the composer's own rule.
		"X1e wider rule below the cut": {
			rows:   []string{"one", rule, devinChip, "", strings.Repeat("─", 81), "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol(devinChip)},
			chip:   chipText,
		},
		// X1f: a full-width-looking rule below a composer whose own top rule
		// is narrower — right shape, wrong composer.
		"X1f rule width from another composer": {
			rows:   []string{"one", strings.Repeat("─", 40), devinChip, "", rule, "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol(devinChip)},
			chip:   chipText,
		},
		// N2c: the same column-0 wrap in Claude's ruled lane — the draft row
		// that ends the composer early hides everything below it as chrome.
		"N2c claude lane wrap-width rule": {
			rows:   []string{"x", rule, claudeChipRow, "", strings.Repeat("─", 40), "  ? for shortcuts"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol(claudeChipRow)},
			chip:   normalizeDelivery(claudeChip),
		},
		// N3b: a ╰───╯ bottom edge whose ─ span is not the box's own.
		"N3b box bottom wrong width": {
			rows:   []string{"x", "╭────────────────╮", "│ > " + codexChip + "     │", "╰────────╯", "  ? for shortcuts"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("│ > " + codexChip + "     │")},
			chip:   chipText,
		},
		// A ruled › lane takes the same proof: when a rule bounds the
		// composer's top, a "─" row of a different width below is a draft
		// row, not Codex's edge.
		"codex ruled lane foreign-width rule": {
			rows:   []string{"one", rule, "› " + codexChip, strings.Repeat("─", 40), "  gpt-5 · ~/repo"},
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("› " + codexChip)},
			chip:   chipText,
		},
	} {
		t.Run(name, func(t *testing.T) {
			pane := strings.Join(tc.rows, "\n") + "\n"
			if ok, why := stillChipComposer(pane, pane, tc.cursor, tc.chip); ok {
				t.Fatalf("ok=true why=%q, want ok=false (the closing edge is not byte-identical to the composer's own top edge)", why)
			}
		})
	}
}

// TestStillChipComposerDevinContinuationIndentIsAnAssumption pins the
// multi-line Devin composer shape the geometry rule ASSUMES — Devin's draft
// continuations indent ("  "), so no draft renders at column 0 inside the
// composer and the rows below the composer's own "─" edge are chrome.
// UNVERIFIED: the repo has no real multi-line Devin capture —
// task/devin_ready_test.go's real captures (devin 3000.2.17, 3000.11.234)
// are all single-line composers, and the stagedDraftPane harness drew this
// indent by construction. If a real Devin draws continuations at column 0,
// only #7912's byte-identical closing edge keeps a wrapped "─" continuation
// from faking the composer's bottom rule (item 2 applies to Devin's lane).
func TestStillChipComposerDevinContinuationIndentIsAnAssumption(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	rule := strings.Repeat("─", 80)
	// The assumed shape: a wrapped Devin chip whose continuation row carries
	// the "  " draft indent, bounded by identical top and bottom rules.
	pane := strings.Join([]string{
		"one",
		rule,
		"❭ [Pasted Content 3207",
		"  chars]",
		"",
		rule,
		"SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits",
	}, "\n") + "\n"
	cursor := paneCursorState{Visible: true, Row: 3, Col: contentEndCol("  chars]")}
	if ok, why := stillChipComposer(pane, pane, cursor, chipText); !ok {
		t.Fatalf("ok=false why=%q, want ok=true (the assumed indented continuation still remedies under the byte-identical edge)", why)
	}
}

// TestStillChipComposerWithholdsDraftsInCodexFooterShape is the #7912 item-1
// class: the Codex model segment is a CLOSED vocabulary — a model token from
// an enumerated family plus only effort-enum tier words and the fixtures'
// enumerated codenames — so a draft line that borrows the "model · path"
// shape without carrying a known model is composer content, not the footer.
// Every frame below remedies on master — the prefix test, the
// two-capitalized-words codename rule, or the warning-word prefix accepts
// it — and must withhold here.
func TestStillChipComposerWithholdsDraftsInCodexFooterShape(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	anchor := "› " + codexChip
	for name, footer := range map[string]string{
		// Y1b: a real model token followed by two capitalized draft words —
		// "Ship It" is not a codename.
		"Y1b known model plus draft words": "  gpt-5 Ship It · ~/repo",
		// Y1c: bare "GPT" is not a family — the head carries no version.
		"Y1c bare GPT plus draft words": "  GPT Deploy Prod · /srv/app",
		// Y1d: a "gpt-" prefix is not a family — no version follows it.
		"Y1d gpt- draft token": "  gpt-please-force-push-release · ~/notes.md",
		// Y1e: a "codex-" prefix is not a family.
		"Y1e codex- draft token": "  codex-then-delete-the-staging-bucket · /srv",
		// Y1f: "GPTs" shares a family's prefix and matches none.
		"Y1f GPTs plural": "  GPTs · ~/notes.md",
		// Y6b: the warning word must be exactly "warning"/"warnings" — a
		// word that merely opens with one is draft text.
		"Y6b warning-word prefix": "  ⚠ 2 warningsforcepush",
	} {
		t.Run(name, func(t *testing.T) {
			pane := strings.Join([]string{anchor, "", footer}, "\n") + "\n"
			cursor := paneCursorState{Visible: true, Row: 0, Col: firstContentCol(anchor)}
			if ok, why := stillChipComposer(pane, pane, cursor, chipText); ok {
				t.Fatalf("ok=true why=%q, want ok=false (%q is not in Codex's closed footer vocabulary)", why, footer)
			}
		})
	}
}

// TestStillChipComposerAcceptsCodexClosedFooterVocabulary pins the other
// side of #7912 item 1: every real Codex footer shape still remedies — a
// known model with a one-token path (Y3c), a Main [token] segment (Y4a),
// the fixtures' codename draws, every tier in codex-rs's ReasoningEffort
// enum (including xhigh, minimal and none, the words the old tier list did
// not know), and each literal/meter segment the grammar owns.
func TestStillChipComposerAcceptsCodexClosedFooterVocabulary(t *testing.T) {
	chipText := normalizeDelivery(codexChip)
	anchor := "› " + codexChip
	end := contentEndCol(anchor)
	for name, footers := range map[string][]string{
		"Y3c known model, one-token path": {"  gpt-5 · ~/repo"},
		"Y4a Main [token] segment":        {"  gpt-5 · ~/repo · Main [default]"},
		"warning plural":                  {"  gpt-5 · ~/repo · ⚠ 2 warnings"},
		"context-left meter":              {"  gpt-5 · ~/repo · 87% context left"},
		"fixture Terra ultra":             {"  GPT-6.1 Terra ultra · ~/sahara/sahara-cheaper-ci · ⚠ 1 warning · f2 to view"},
		"fixture Astra":                   {"  GPT-6-Astra ultra · ~/sahara/sahara-clean · Pursuing goal · Main [default]"},
		"fixture sol max":                 {"  gpt-5.6-sol max · ~/agent-factory"},
		"fixture luna low":                {"  gpt-5.6-luna low · ~/agent-factory"},
		"effort none":                     {"  gpt-5.2-codex none · ~/repo"},
		"effort minimal":                  {"  gpt-5.1-codex minimal · ~/repo"},
		"effort xhigh":                    {"  gpt-5.1-codex-max xhigh · ~/repo"},
		"effort persistent":               {"  gpt-5.2-codex persistent · ~/repo"},
		"o-series mini":                   {"  o4-mini medium · ~/repo"},
		"codex-mini line":                 {"  codex-mini-latest low · ~/repo"},
		"hints row":                       {"  ← for agents · ? for shortcuts ⚠ 1 warning · f2 to view"},
		"esc interrupt row":               {"  esc to interrupt"},
	} {
		t.Run(name, func(t *testing.T) {
			rows := append([]string{anchor, ""}, footers...)
			pane := strings.Join(rows, "\n") + "\n"
			cursor := paneCursorState{Visible: true, Row: 0, Col: end}
			if ok, why := stillChipComposer(pane, pane, cursor, chipText); !ok {
				t.Fatalf("ok=false why=%q, want ok=true (every footer row is in Codex's closed vocabulary)", why)
			}
		})
	}
}

// TestStillChipComposerAcceptsEveryRealAgentFooter pins the whole-row
// grammars against the footers the agents actually draw: every row below the
// composer is provably that agent's own, so the chip-only composer still
// remedies.
func TestStillChipComposerAcceptsEveryRealAgentFooter(t *testing.T) {
	rule := strings.Repeat("─", 80)
	for name, tc := range map[string]struct {
		pane   string
		cursor paneCursorState
		chip   string
	}{
		// Codex's ruled lane: the rule below the composer, then the
		// model/goal status line and the shortcut hints line.
		"codex ruled lane": {
			pane: strings.Join([]string{
				"  Worked for 1m 32s — 02:01", "",
				"› " + codexChip,
				rule,
				"  GPT-6-Astra ultra · ~/sahara/sahara-clean · Pursuing goal · Main [default]",
				"  ← for agents · ? for shortcuts ⚠ 1 warning · f2 to view",
			}, "\n") + "\n",
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("› " + codexChip)},
			chip:   normalizeDelivery(codexChip),
		},
		// Devin: ruled composer, the model/mode/shells footer directly
		// below the bottom rule.
		"devin ruled footer": {
			pane: strings.Join([]string{
				"one",
				rule,
				"❭ " + codexChip,
				rule,
				"SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits",
			}, "\n") + "\n",
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("❭ " + codexChip)},
			chip:   normalizeDelivery(codexChip),
		},
		// Claude's ruled composer with the shortcuts hint row.
		"claude ? for shortcuts": {
			pane: strings.Join([]string{
				"x",
				rule,
				"❯ " + claudeChip,
				rule,
				"",
				"  ? for shortcuts",
			}, "\n") + "\n",
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("❯ " + claudeChip)},
			chip:   normalizeDelivery(claudeChip),
		},
		// Claude's idle permission footer.
		"claude auto mode footer": {
			pane: strings.Join([]string{
				"x",
				rule,
				"❯ " + claudeChip,
				rule,
				"",
				"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
			}, "\n") + "\n",
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("❯ " + claudeChip)},
			chip:   normalizeDelivery(claudeChip),
		},
		// Claude's │-boxed composer: the ">" first-row prompt inside the
		// box, the box edge below, then Claude's own footer.
		"claude boxed composer": {
			pane: strings.Join([]string{
				"x",
				"╭────────────────╮",
				"│ > " + codexChip + "     │",
				"╰────────────────╯",
				"  ? for shortcuts",
			}, "\n") + "\n",
			cursor: paneCursorState{Visible: true, Row: 2, Col: contentEndCol("│ > " + codexChip)},
			chip:   normalizeDelivery(codexChip),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, why := stillChipComposer(tc.pane, tc.pane, tc.cursor, tc.chip); !ok {
				t.Fatalf("ok=false why=%q, want ok=true (every row below the composer is provably the agent's own footer)", why)
			}
		})
	}
}

// TestStillChipComposerAcceptsClaudeFootersBelowTheClosingRule is the #7850
// N2 regression class: the vim-mode composer is the only Claude composer
// that reaches the remedy, and its footer rows sit BELOW the closing "─"
// rule — chrome by geometry, where no draft can draw. Plan mode, the
// auto-compact meter and a custom statusLine carry no phrase the grammars
// knew, and must remedy again.
func TestStillChipComposerAcceptsClaudeFootersBelowTheClosingRule(t *testing.T) {
	rule := strings.Repeat("─", 80)
	chipText := normalizeDelivery(claudeChip)
	for name, footer := range map[string]string{
		"K12b plan mode":           "  ⏸ plan mode on (shift+tab to cycle)",
		"K12c auto-compact meter":  "  Context left until auto-compact: 12%",
		"K12d custom status line":  "  my-org / api · main ✻",
		"INSERT + agent count":     "  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent",
		"auto mode + for agents":   "  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
		"? for shortcuts":          "  ? for shortcuts",
		"arbitrary chrome wording": "  anything the agent draws here",
	} {
		t.Run(name, func(t *testing.T) {
			pane := strings.Join([]string{
				"x", rule, "❯ " + claudeChip, rule, "", footer,
			}, "\n") + "\n"
			cursor := paneCursorState{Visible: true, Row: 2, Col: contentEndCol("❯ " + claudeChip)}
			if ok, why := stillChipComposer(pane, pane, cursor, chipText); !ok {
				t.Fatalf("ok=false why=%q, want ok=true (rows below Claude's closing rule are chrome by geometry)", why)
			}
		})
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
	if ok, why := stillChipComposer(pane, pane, paneCursorState{Visible: true, Row: 0, Col: end}, chipText); !ok {
		t.Fatalf("codex warning footer below a blank row: ok=false why=%q, want ok=true (the footer is provably the agent's own)", why)
	}

	// The warning-free default Codex status line — "model · path" with no
	// marker phrase — is the same provable footer: codexStatusLineModel owns
	// its shape, so the composer is still provably chip-only.
	defaultStatus := strings.Join([]string{
		"› " + codexChip,
		"",
		"  gpt-5.6-sol max · ~/agent-factory",
	}, "\n") + "\n"
	if ok, why := stillChipComposer(defaultStatus, defaultStatus, paneCursorState{Visible: true, Row: 0, Col: end}, chipText); !ok {
		t.Fatalf("warning-free codex status footer below a blank row: ok=false why=%q, want ok=true (the model/path footer is provably the agent's own)", why)
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
