package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The caret is the reverse-video SGR (ESC[7m) wrapping a single cell. Asserting on
// the sequence rather than a rendered glyph keeps the test honest about what makes
// the caret visible in a terminal.
const reverseVideoSGR = "\x1b[7m"

// forceProfile pins the lipgloss colour profile for one test. Rendering is
// profile-dependent (termenv emits no sequences under Ascii) and the profile is
// process-wide, so it must be restored.
func forceProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// TestInputCaretIsStaticReverseVideo covers #1826 item 7. The inline inputs used to
// append a literal "_", which is indistinguishable from a typed underscore. The
// replacement is a reverse-video cell — and it is STATIC: no blink, per the
// no-animation doctrine of #1766.
func TestInputCaretIsStaticReverseVideo(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	caret := InputCaret()
	if !strings.Contains(caret, reverseVideoSGR) {
		t.Errorf("want a reverse-video caret, got %q", caret)
	}
	if strings.Contains(caret, "_") {
		t.Errorf("the literal underscore caret must be gone, got %q", caret)
	}
	// Same input, same output: nothing frame-dependent to animate it.
	if InputCaret() != caret {
		t.Error("InputCaret must be static; it rendered two different values")
	}
}

// TestInputCaretVisibleWithoutColor guards the degradation. Under the Ascii profile
// termenv emits no SGR at all, so a reverse-video space would collapse to a bare
// space and the caret would silently vanish on a TERM=dumb / NO_COLOR terminal.
func TestInputCaretVisibleWithoutColor(t *testing.T) {
	forceProfile(t, termenv.Ascii)

	if caret := InputCaret(); strings.TrimSpace(caret) == "" {
		t.Errorf("the caret must stay visible without colour support, got %q", caret)
	}
}

// TestInputCaretIsOneCell keeps the caret from shifting the text it trails, under
// either profile.
func TestInputCaretIsOneCell(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile termenv.Profile
	}{
		{"colour", termenv.TrueColor},
		{"ascii", termenv.Ascii},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forceProfile(t, tc.profile)
			// lipgloss.Width measures rendered cells, discounting the SGR wrapper.
			if w := lipgloss.Width(InputCaret()); w != 1 {
				t.Errorf("want a 1-cell caret, got %d", w)
			}
		})
	}
}

// TestHooksPaneRendersCaretNotUnderscore checks the caret reaches the hooks-pane
// editor rather than only existing as a helper.
func TestHooksPaneRendersCaretNotUnderscore(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	h := NewHooksPane()
	// String() fits its block to the pane size; unsized it collapses to blank lines.
	h.SetSize(60, 12)
	h.SetCommands([]string{"make test"})
	h.SetFocus(true)
	h.editing = true
	h.editBuffer = "make lint"

	out := h.String()
	if !strings.Contains(out, reverseVideoSGR) {
		t.Errorf("want the edit line to carry a reverse-video caret, got %q", out)
	}
	if strings.Contains(out, "make lint_") {
		t.Errorf("the edit buffer must not carry a literal _ caret, got %q", out)
	}
}
