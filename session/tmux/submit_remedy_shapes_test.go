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
	codexRow := "› " + codexChip
	ok, why = stillChipComposer(codex, codex, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, normalizeDelivery(codexChip))
	if !ok {
		t.Fatalf("codex composer with a warning footer: withheld (%s)", why)
	}
	// ...while a footer drawn with no blank row between it and the composer is
	// composer content as far as the region goes, and the reason names that.
	tight := codexWarningPane("› "+codexChip, false)
	ok, why = stillChipComposer(tight, tight, paneCursorState{Visible: true, Row: 2, Col: len([]rune(codexRow))}, normalizeDelivery(codexChip))
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
	anchor := lastComposerGlyphRow(paneGridRows(staged))
	if !boundStillStaged(staged, deliveryProbe{}, bound, anchor) {
		t.Fatal("a chip still in the composer must read as staged")
	}
	if boundStillStaged(submitted, deliveryProbe{}, bound, anchor) {
		t.Fatal("a bare repainted composer above an INSERT status line must read as submitted")
	}
}
