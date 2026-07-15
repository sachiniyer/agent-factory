package overlay

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/sachiniyer/agent-factory/ui"
)

// forceProfile pins the process-wide lipgloss colour profile for one test, so the
// caret renders its styled form rather than the no-colour fallback.
func forceProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// TestSearchOverlayRendersCaretNotUnderscore covers #1826 item 7 at the search
// overlay: the query line trailed a literal "_", which read as a typed character.
func TestSearchOverlayRendersCaretNotUnderscore(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	s := NewSearchOverlay(nil)
	s.query = "gamma"

	out := s.Render()
	if !strings.Contains(out, ui.InputCaret()) {
		t.Errorf("want the query line to carry the shared caret, got %q", out)
	}
	if strings.Contains(out, "gamma_") {
		t.Errorf("the query must not carry a literal _ caret, got %q", out)
	}
}

// TestProjectPickerRendersCaretNotUnderscore covers the same for the picker's
// add-project path input.
func TestProjectPickerRendersCaretNotUnderscore(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	p := NewProjectPickerOverlay(nil, "")
	p.adding = true
	p.pathInput = "/home/u/code/repo"

	out := p.Render()
	if !strings.Contains(out, ui.InputCaret()) {
		t.Errorf("want the path line to carry the shared caret, got %q", out)
	}
	if strings.Contains(out, "repo_") {
		t.Errorf("the path input must not carry a literal _ caret, got %q", out)
	}
}

// The two overlays are the same KIND of input, so they must not drift apart again:
// both take their caret from the one helper rather than open-coding a marker.
func TestOverlayCaretsAreTheSameGlyph(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	s := NewSearchOverlay(nil)
	s.query = "q"
	p := NewProjectPickerOverlay(nil, "")
	p.adding = true
	p.pathInput = "q"

	caret := ui.InputCaret()
	if !strings.Contains(s.Render(), caret) || !strings.Contains(p.Render(), caret) {
		t.Error("both overlays must render the shared ui.InputCaret")
	}
}
