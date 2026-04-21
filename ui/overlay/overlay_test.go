package overlay

import (
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
