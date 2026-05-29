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
	lipgloss.SetColorProfile(termenv.ANSI256)

	style := lipgloss.NewStyle().
		Background(lipgloss.Color("#dde4f0")).
		Foreground(lipgloss.Color("#1a1a1a"))

	bg := style.Render("Selected Item")
	// Produces: "\x1b[38;5;232;48;5;189mSelected Item\x1b[0m"

	if !strings.Contains(bg, "38;5;232;48;5;") {
		t.Fatalf("Setup error: expected combined FG+BG sequence, got: %q", bg)
	}

	result := PlaceOverlay(0, 0, "XX", bg, false)

	if !strings.Contains(result, "48;5;236") {
		t.Fatalf("expected background to be faded to gray 236, got: %q", result)
	}
	if !strings.Contains(result, "38;5;240") {
		t.Fatalf("BUG CONFIRMED (#701): input had both 38;5 (fg) and 48;5 (bg), "+
			"but output dropped the faded foreground. Got: %q", result)
	}
}

// TestFadeSGR exercises fadeSGR directly across the SGR shapes that flow
// through the overlay fade path. Each case asserts the exact faded sequence so
// that both colors of a combined FG+BG input are provably preserved (#701) and
// the pre-existing FG-only / BG-only / attribute behavior is unchanged.
func TestFadeSGR(t *testing.T) {
	const (
		fg = "\x1b[38;5;240m"
		bg = "\x1b[48;5;236m"
	)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"reset", "\x1b[0m", "\x1b[0m"},
		{"empty-reset", "\x1b[m", "\x1b[m"},

		{"bare-fg-256", "\x1b[38;5;232m", fg},
		{"bare-fg-basic", "\x1b[37m", fg},
		{"bare-fg-bright", "\x1b[97m", fg},
		{"bare-fg-truecolor", "\x1b[38;2;10;20;30m", fg},

		{"bare-bg-256", "\x1b[48;5;189m", bg},
		{"bare-bg-basic", "\x1b[41m", bg},
		{"bare-bg-bright", "\x1b[101m", bg},
		{"bare-bg-truecolor", "\x1b[48;2;200;210;220m", bg},
		{"reverse-video", "\x1b[7m", bg},

		{"combined-256", "\x1b[38;5;232;48;5;189m", "\x1b[38;5;240;48;5;236m"},
		{"combined-256-reversed", "\x1b[48;5;189;38;5;232m", "\x1b[38;5;240;48;5;236m"},
		{"combined-truecolor", "\x1b[38;2;10;20;30;48;2;200;210;220m", "\x1b[38;5;240;48;5;236m"},
		{"combined-mixed", "\x1b[38;5;232;48;2;200;210;220m", "\x1b[38;5;240;48;5;236m"},

		{"bold-fg-256", "\x1b[1;38;5;188m", fg},
		{"bold-italic-combined", "\x1b[1;3;38;5;232;48;5;189m", "\x1b[38;5;240;48;5;236m"},
		{"bold-bg", "\x1b[1;41m", bg},
		{"bold-only", "\x1b[1m", fg},
		{"underline-fg", "\x1b[4;32m", fg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fadeSGR(tc.in); got != tc.want {
				t.Fatalf("fadeSGR(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFadeSGRTrueColorCombinedThroughOverlay drives the truecolor combined case
// end-to-end through PlaceOverlay to confirm both colors survive there too.
func TestFadeSGRTrueColorCombinedThroughOverlay(t *testing.T) {
	input := "\x1b[38;2;10;20;30;48;2;200;210;220mhi\x1b[0m"
	result := PlaceOverlay(0, 0, "XX", input, false)

	if !strings.Contains(result, "38;5;240") {
		t.Fatalf("truecolor combined: faded foreground missing, got: %q", result)
	}
	if !strings.Contains(result, "48;5;236") {
		t.Fatalf("truecolor combined: faded background missing, got: %q", result)
	}
}
