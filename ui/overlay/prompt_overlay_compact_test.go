package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

// #5587: the jump-to-tab prompt reuses this overlay for ONE line of input, and
// without SetCompact the textarea grew to fill the whole fit frame, so a
// three-answer dialog rendered as a screen of blank rows. Compact mode keeps
// the textarea at its content height and the dialog hugs the field.
func TestPromptOverlayCompactSizesToContent(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	full := NewPromptOverlay("Jump to tab (number or name)", "")
	full.SetHints("enter jump · esc cancel", "enter jump · esc cancel")
	full.SetMaxSize(80, 24)
	fullRows := strings.Count(renderedText(full.Render()), "\n") + 1

	compact := NewPromptOverlay("Jump to tab (number or name)", "")
	compact.SetHints("enter jump · esc cancel", "enter jump · esc cancel")
	compact.SetCompact()
	compact.SetMaxSize(80, 24)
	got := renderedText(compact.Render())
	compactRows := strings.Count(got, "\n") + 1

	if compactRows >= fullRows {
		t.Errorf("compact prompt should hug its content: %d rows vs the full-frame %d:\n%s",
			compactRows, fullRows, got)
	}
	if compactRows > 10 {
		t.Errorf("a one-line prompt should render near content size, got %d rows:\n%s",
			compactRows, got)
	}
}

// #5587: the jump field shows its live answer on a status line between the
// input and the hint — the tab the query would land on, or "no match". The
// overlay owns only the row; the caller owns the text.
func TestPromptOverlayRendersStatusLine(t *testing.T) {
	forceProfile(t, termenv.TrueColor)

	p := NewPromptOverlay("Jump to tab (number or name)", "")
	p.SetHints("enter jump · esc cancel", "enter jump · esc cancel")
	p.SetCompact()
	p.SetMaxSize(80, 24)

	if got := renderedText(p.Render()); strings.Contains(got, "no match") {
		t.Errorf("status line must stay hidden until set:\n%s", got)
	}
	p.SetStatus("→ Terminal")
	if got := renderedText(p.Render()); !strings.Contains(got, "→ Terminal") {
		t.Errorf("the live target must render under the field:\n%s", got)
	}
	p.SetStatus("no match")
	if got := renderedText(p.Render()); !strings.Contains(got, "no match") {
		t.Errorf("a miss must be just as visible as a target:\n%s", got)
	}
	p.SetStatus("")
	if got := renderedText(p.Render()); strings.Contains(got, "no match") {
		t.Errorf("clearing the status must remove the row:\n%s", got)
	}
}
