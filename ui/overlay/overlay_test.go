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
