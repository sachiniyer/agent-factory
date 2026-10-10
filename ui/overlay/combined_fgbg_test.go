package overlay

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestPlaceOverlayCombinedFgBgFade is the headline regression guard for #701:
// a combined foreground+background SGR sequence emitted by lipgloss must keep
// BOTH colors when faded. The pre-fix code over-matched the combined sequence
// with bgColorRegex and replaced it with a background-only gray, dropping the
// foreground entirely.
func TestPlaceOverlayCombinedFgBgFade(t *testing.T) {
	forceProfile(t, termenv.ANSI256)

	style := lipgloss.NewStyle().
		Background(lipgloss.Color("#dde4f0")).
		Foreground(lipgloss.Color("#1a1a1a"))

	bg := style.Render("Selected Item")
	// Produces: "\x1b[38;5;232;48;5;189mSelected Item\x1b[0m"

	if !strings.Contains(bg, "38;5;232;48;5;") {
		t.Fatalf("Setup error: expected combined FG+BG sequence, got: %q", bg)
	}

	result := PlaceOverlay(0, 0, "XX", bg, false)

	if !strings.Contains(result, testBackdropBG()) {
		t.Fatalf("expected background to be resolved to the surface role, got: %q", result)
	}
	if !strings.Contains(result, testBackdropFG()) {
		t.Fatalf("BUG CONFIRMED (#701): input had both 38;5 (fg) and 48;5 (bg), "+
			"but output dropped the faded foreground. Got: %q", result)
	}
}

// TestFadeSGRTrueColorCombinedThroughOverlay drives the truecolor combined case
// end-to-end through PlaceOverlay to confirm both colors survive there too.
func TestFadeSGRTrueColorCombinedThroughOverlay(t *testing.T) {
	input := "\x1b[38;2;10;20;30;48;2;200;210;220mhi\x1b[0m"
	result := PlaceOverlay(0, 0, "XX", input, false)

	if !strings.Contains(result, testBackdropFG()) {
		t.Fatalf("truecolor combined: faded foreground missing, got: %q", result)
	}
	if !strings.Contains(result, testBackdropBG()) {
		t.Fatalf("truecolor combined: faded background missing, got: %q", result)
	}
}

func testBackdropFG() string { fg, _ := backdropColors(); return fg }
func testBackdropBG() string { _, bg := backdropColors(); return bg }
