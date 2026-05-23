package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/ansi"
)

// TestWhitespaceRenderWideCharExceedsWidth verifies that render() does not
// over-emit cells when the whitespace pattern mixes wide and narrow runes.
// Previously the loop counted the width of the next rune (rather than the one
// it just wrote), which made termination off-by-one for CJK patterns.
func TestWhitespaceRenderWideCharExceedsWidth(t *testing.T) {
	ws := &whitespace{chars: "界a"} // 界 is 2 cells wide, 'a' is 1 cell
	result := ws.render(2)
	width := ansi.PrintableRuneWidth(result)
	if width != 2 {
		t.Fatalf("Expected width 2, got %d, result=%q", width, result)
	}
}

// TestWhitespaceRenderASCII verifies the common single-space case still fills
// the requested width exactly.
func TestWhitespaceRenderASCII(t *testing.T) {
	ws := &whitespace{}
	result := ws.render(5)
	width := ansi.PrintableRuneWidth(result)
	if width != 5 {
		t.Fatalf("Expected width 5, got %d, result=%q", width, result)
	}
}

// TestWhitespaceRenderWideCharEvenWidth verifies a pure wide-char pattern
// fills an even width exactly using two runes.
func TestWhitespaceRenderWideCharEvenWidth(t *testing.T) {
	ws := &whitespace{chars: "界"}
	result := ws.render(4)
	width := ansi.PrintableRuneWidth(result)
	if width != 4 {
		t.Fatalf("Expected width 4, got %d, result=%q", width, result)
	}
}

// TestPlaceOverlayEqualWidth verifies that when the foreground width exactly
// matches the background width (a common case with hardcoded 60-col overlays
// in 60-col tmux panes), the dimmed background is still composited above and
// below the foreground rather than the function early-returning the foreground
// alone. Regression test for #322.
func TestPlaceOverlayEqualWidth(t *testing.T) {
	bg := strings.Join([]string{
		"aaaaaaaaaa",
		"bbbbbbbbbb",
		"cccccccccc",
		"dddddddddd",
	}, "\n")
	fg := strings.Join([]string{
		"XXXXXXXXXX",
		"YYYYYYYYYY",
	}, "\n")

	result := PlaceOverlay(0, 1, fg, bg, false)

	// The result must span all 4 background rows, not just the 2 foreground
	// rows. Prior to the fix, PlaceOverlay early-returned `fg` (2 lines) when
	// fgWidth == bgWidth.
	gotLines := strings.Split(result, "\n")
	if len(gotLines) != 4 {
		t.Fatalf("Expected 4 lines (background height), got %d: %q", len(gotLines), result)
	}

	// The foreground content should appear in the result somewhere.
	if !strings.Contains(result, "XXXXXXXXXX") || !strings.Contains(result, "YYYYYYYYYY") {
		t.Fatalf("Foreground content missing from composite result: %q", result)
	}

	// At least one background row's content (the 'a' or 'd' rows that the
	// foreground does not cover) must appear in the result, indicating that
	// the dimmed background was composited rather than dropped.
	if !strings.Contains(result, "aaaaaaaaaa") && !strings.Contains(result, "dddddddddd") {
		t.Fatalf("Dimmed background content missing from composite result: %q", result)
	}
}

// TestPlaceOverlayEqualHeight verifies the height-equal case symmetrically.
// Regression test for #322.
func TestPlaceOverlayEqualHeight(t *testing.T) {
	bg := strings.Join([]string{
		"aaaaaaaaaa",
		"bbbbbbbbbb",
		"cccccccccc",
	}, "\n")
	fg := strings.Join([]string{
		"XXXX",
		"YYYY",
		"ZZZZ",
	}, "\n")

	result := PlaceOverlay(0, 0, fg, bg, false)

	gotLines := strings.Split(result, "\n")
	if len(gotLines) != 3 {
		t.Fatalf("Expected 3 lines (background height), got %d: %q", len(gotLines), result)
	}

	// Each line should be at least as wide as the background (the foreground
	// only covers the leftmost 4 cells, so the right side must come from the
	// dimmed background).
	for i, line := range gotLines {
		if ansi.PrintableRuneWidth(line) < 10 {
			t.Fatalf("Line %d shorter than background width: %q (width %d)", i, line, ansi.PrintableRuneWidth(line))
		}
	}
}

func TestPlaceOverlayBasicBackgroundFade(t *testing.T) {
	result := PlaceOverlay(0, 0, "XX", "\x1b[41mred-bg\x1b[0m", false)

	if strings.Contains(result, "\x1b[38;5;240m") {
		t.Fatalf("basic background code was faded as foreground: %q", result)
	}
	if !strings.Contains(result, "\x1b[48;5;236m") {
		t.Fatalf("expected faded background code, got: %q", result)
	}
}

func TestPlaceOverlayReverseVideoFadesAsBackground(t *testing.T) {
	result := PlaceOverlay(0, 0, "XX", "\x1b[7mreverse\x1b[0m", false)

	if !strings.Contains(result, "\x1b[48;5;236m") {
		t.Fatalf("expected reverse-video styling to preserve background semantics, got: %q", result)
	}
}

// TestPlaceOverlaySingleParamForegroundFade verifies the pre-existing
// single-parameter case (e.g. \x1b[37m) is still faded to the medium-gray
// foreground. Regression guard for the regex broadening in #469.
func TestPlaceOverlaySingleParamForegroundFade(t *testing.T) {
	result := PlaceOverlay(0, 0, "XX", "\x1b[37mhello\x1b[0m", false)

	if !strings.Contains(result, "\x1b[38;5;240m") {
		t.Fatalf("expected single-param fg code to be faded, got: %q", result)
	}
	if strings.Contains(result, "\x1b[37m") {
		t.Fatalf("original \\x1b[37m should have been rewritten, got: %q", result)
	}
	if !strings.Contains(result, "\x1b[0m") {
		t.Fatalf("trailing reset should be preserved, got: %q", result)
	}
}

// TestPlaceOverlayMultiParamForegroundFade covers the #469 bug: lipgloss in
// 16-color mode emits multi-parameter SGR sequences like \x1b[1;37m (bold +
// white). The original regex only matched single-parameter sequences, so these
// passed through the fade logic unchanged and stayed visible under overlays.
func TestPlaceOverlayMultiParamForegroundFade(t *testing.T) {
	result := PlaceOverlay(0, 0, "XX", "\x1b[1;37mhello\x1b[0m", false)

	if strings.Contains(result, "\x1b[1;37m") {
		t.Fatalf("multi-param SGR \\x1b[1;37m leaked through fade: %q", result)
	}
	if !strings.Contains(result, "\x1b[38;5;240m") {
		t.Fatalf("expected multi-param fg code to be faded, got: %q", result)
	}
	if !strings.Contains(result, "\x1b[0m") {
		t.Fatalf("trailing reset should be preserved, got: %q", result)
	}
}

// TestPlaceOverlayMultiParamBackgroundFade verifies that a multi-parameter SGR
// whose parameters include a background-color code is faded to the dark-gray
// background rather than the foreground gray.
func TestPlaceOverlayMultiParamBackgroundFade(t *testing.T) {
	result := PlaceOverlay(0, 0, "XX", "\x1b[1;41mbold-red-bg\x1b[0m", false)

	if strings.Contains(result, "\x1b[1;41m") {
		t.Fatalf("multi-param SGR \\x1b[1;41m leaked through fade: %q", result)
	}
	if !strings.Contains(result, "\x1b[48;5;236m") {
		t.Fatalf("expected multi-param bg code to be faded to bg gray, got: %q", result)
	}
}

// TestPlaceOverlayExtendedBackgroundFade is a regression guard for #564:
// step 1 of PlaceOverlay rewrites \x1b[48;5;Nm to \x1b[48;5;236m, but
// simpleColorRegex then re-matched that rewrite and (because SGR 48 was
// missing from the background detector) re-faded it as foreground gray.
func TestPlaceOverlayExtendedBackgroundFade(t *testing.T) {
	input := "\x1b[48;5;196mred-bg\x1b[0m"
	result := PlaceOverlay(0, 0, "XX", input, false)

	if strings.Contains(result, "\x1b[38;5;240m") && strings.Contains(input, "\x1b[48;5;") {
		t.Fatalf("BUG CONFIRMED: extended background was faded as foreground.\nInput had 48;5 (bg), output has 38;5 (fg)")
	}
}

// TestPlaceOverlayTrueColorBackgroundFade is the true-color (24-bit, 48;2;R;G;B)
// counterpart to TestPlaceOverlayExtendedBackgroundFade — same #564 bug,
// different input shape.
func TestPlaceOverlayTrueColorBackgroundFade(t *testing.T) {
	input := "\x1b[48;2;255;0;0mtrue-color-red-bg\x1b[0m"
	result := PlaceOverlay(0, 0, "XX", input, false)

	if strings.Contains(result, "\x1b[38;5;240m") && strings.Contains(input, "\x1b[48;2;") {
		t.Fatalf("BUG CONFIRMED: true-color background was faded as foreground")
	}
}

// TestPlaceOverlayCJKWideCharStraddle is a regression guard for #647. When the
// position after the foreground falls inside a wide (CJK) grapheme,
// xansi.TruncateLeft preserves the entire grapheme — returning a string whose
// printable width is 1 cell larger than the remaining background space. The
// pre-fix code wrote that right-side slab unconditionally and skipped padding,
// producing a line 1 cell wider than the background.
func TestPlaceOverlayCJKWideCharStraddle(t *testing.T) {
	// bg: 7 ASCII + three 2-cell CJK = width 13.
	// fg width 4 placed at x=4 → pos=8 lands inside the first 界.
	bg := "abcdefg界界界"
	fg := "XXXX"
	bgWidth := ansi.PrintableRuneWidth(bg)

	result := PlaceOverlay(4, 0, fg, bg, false)

	gotWidth := ansi.PrintableRuneWidth(result)
	if gotWidth != bgWidth {
		t.Fatalf("PlaceOverlay overflow: got width %d, want %d (bg=%q result=%q)",
			gotWidth, bgWidth, bg, result)
	}
}

// TestPlaceOverlayCJKWideCharStraddleCentered exercises the centered variant
// from the bug report: centering a width-2 fg on a width-12 bg yields placeX=5,
// so pos=7 lands inside a CJK glyph at cells 7-8.
func TestPlaceOverlayCJKWideCharStraddleCentered(t *testing.T) {
	// bg: 6 ASCII + three 2-cell CJK = width 12. pos after fg = 5+2 = 7.
	bg := "abcdef界界界"
	fg := "YY"
	bgWidth := ansi.PrintableRuneWidth(bg)

	result := PlaceOverlay(0, 0, fg, bg, true)

	gotWidth := ansi.PrintableRuneWidth(result)
	if gotWidth != bgWidth {
		t.Fatalf("PlaceOverlay centered overflow: got width %d, want %d (bg=%q result=%q)",
			gotWidth, bgWidth, bg, result)
	}
}

// TestPlaceOverlayASCIINoRegression is a guard that the #647 fix doesn't change
// the ASCII-only path, where rightWidth always equals remainingWidth exactly.
func TestPlaceOverlayASCIINoRegression(t *testing.T) {
	bg := "abcdefghij" // width 10
	fg := "XX"
	bgWidth := ansi.PrintableRuneWidth(bg)

	result := PlaceOverlay(3, 0, fg, bg, false)

	gotWidth := ansi.PrintableRuneWidth(result)
	if gotWidth != bgWidth {
		t.Fatalf("ASCII PlaceOverlay width changed: got %d, want %d (result=%q)",
			gotWidth, bgWidth, result)
	}
	if !strings.Contains(result, "abc") || !strings.Contains(result, "XX") || !strings.Contains(result, "fghij") {
		t.Fatalf("ASCII composition incorrect: %q", result)
	}
}

// TestPlaceOverlayCJKAlignedNoTruncation verifies that when pos lands cleanly
// on a CJK grapheme boundary (even pos in this layout), TruncateLeft already
// returns the exact remaining width and no re-truncation is needed. This guards
// against the fix accidentally trimming a cell in the well-behaved case.
func TestPlaceOverlayCJKAlignedNoTruncation(t *testing.T) {
	// 6 ASCII + three 2-cell CJK = width 12. fg width 2 placed at x=6 →
	// pos=8 lands exactly between the first and second CJK glyph.
	bg := "abcdef界界界"
	fg := "ZZ"
	bgWidth := ansi.PrintableRuneWidth(bg)

	result := PlaceOverlay(6, 0, fg, bg, false)

	gotWidth := ansi.PrintableRuneWidth(result)
	if gotWidth != bgWidth {
		t.Fatalf("CJK-aligned PlaceOverlay width changed: got %d, want %d (result=%q)",
			gotWidth, bgWidth, result)
	}
	// Both trailing CJK glyphs should survive since pos sits on the boundary.
	if !strings.Contains(result, "界界") {
		t.Fatalf("expected trailing CJK glyphs preserved, got: %q", result)
	}
}
