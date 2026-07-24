package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestConfigPaneAssistantButtonRequestsAndCloses is the #2453 button. Pressing C
// in normal mode records an assistant request AND closes the pane: the pane
// cannot spawn the agent itself (that is a daemon round trip owned by the app),
// so it signals the host and steps aside. TakeAssistantRequest returns the
// request once and clears it.
func TestConfigPaneAssistantButtonRequestsAndCloses(t *testing.T) {
	c := newTestConfigPane(t)

	if c.TakeAssistantRequest() {
		t.Fatal("a fresh pane must not have a pending assistant request")
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})

	if c.HasFocus() {
		t.Error("pressing C must close the config pane so the host can take over the terminal")
	}
	if !c.TakeAssistantRequest() {
		t.Fatal("pressing C must record an assistant request for the host to act on")
	}
	// Read-and-clear: the request fires exactly once, so a second read is false and
	// the host cannot re-spawn on a later, unrelated close.
	if c.TakeAssistantRequest() {
		t.Error("the assistant request must clear when taken — a second read must be false")
	}
}

// TestConfigPaneAssistantRequestClearsOnAnUnrelatedClose pins that a request
// cannot leak across opens. If the pane is dismissed some other way (Esc) after C
// was somehow set but never taken, reopening must not inherit the stale intent.
func TestConfigPaneAssistantRequestClearsOnAnUnrelatedClose(t *testing.T) {
	c := newTestConfigPane(t)

	// Directly arm the flag, then close via Esc without the host taking it.
	c.assistantRequested = true
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})

	if c.TakeAssistantRequest() {
		t.Error("an Esc close must drop a pending assistant request, so a later open cannot inherit it")
	}
}

// TestConfigPaneShowsTheAssistantButtonHint pins the affordance is discoverable:
// the button is only a button if the user can see it. The normal-mode hint row
// must advertise C.
func TestConfigPaneShowsTheAssistantButtonHint(t *testing.T) {
	c := newTestConfigPane(t)
	view := c.String()
	if !strings.Contains(view, "C assistant") {
		t.Errorf("the config pane must advertise the assistant button in its hints.\n--- view ---\n%s", view)
	}
}

// TestConfigPaneHintFitsTheBoxAndKeepsTheButton is the #1936 width guard for the
// added hint. At the real overlay inner width (~60), the full hint row would be
// one column too long and wrap "esc close" onto a second line. The row must
// instead fit on ONE line, and the two hints that must never be shed — the
// assistant button and the exit — must survive; the advanced toggle is the one
// that drops.
func TestConfigPaneHintFitsTheBoxAndKeepsTheButton(t *testing.T) {
	c := newTestConfigPane(t)
	const inner = 60 // the config overlay's real inner content width (box target 64)
	c.SetSize(inner, 40)

	hint := c.renderHints()
	for _, line := range strings.Split(hint, "\n") {
		if w := lipgloss.Width(line); w > inner {
			t.Errorf("hint line is %d cols wide, past the %d-col box — it will wrap:\n%q", w, inner, line)
		}
	}
	if !strings.Contains(hint, "C assistant") {
		t.Error("the assistant button must survive the width squeeze — it is the headline affordance")
	}
	if !strings.Contains(hint, "esc close") {
		t.Error("the exit hint must survive the width squeeze")
	}
	// The dropped one is the advanced toggle, and `a` still works regardless.
	if strings.Contains(hint, "advanced") {
		t.Error("at this width the advanced toggle should have been shed to make room")
	}
}

// TestConfigPaneShowsAllHintsWhenWide pins the other side: given room, nothing is
// dropped — the advanced toggle returns alongside the assistant button.
func TestConfigPaneShowsAllHintsWhenWide(t *testing.T) {
	c := newTestConfigPane(t) // sized 100 wide by the helper
	hint := c.renderHints()
	for _, want := range []string{"advanced", "C assistant", "esc close"} {
		if !strings.Contains(hint, want) {
			t.Errorf("a wide box must show every hint, missing %q:\n%q", want, hint)
		}
	}
}

// TestConfigPaneAssistantKeyIsInertWhileEditing guards the one collision: C is a
// perfectly ordinary character to type into a value field (a path under
// /home/carol, a branch prefix). While a value is being edited, C must reach the
// field as text and must NOT open the assistant.
func TestConfigPaneAssistantKeyIsInertWhileEditing(t *testing.T) {
	c := newTestConfigPane(t)
	c.beginEdit()
	if !c.IsEditing() {
		t.Fatal("precondition: a value field must be open")
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})

	if c.TakeAssistantRequest() {
		t.Error("C typed into a value field must be text, not the assistant button")
	}
	if !c.IsEditing() {
		t.Error("typing C must not close the edit field")
	}
	if !strings.Contains(c.EditValueForTest(), "C") {
		t.Errorf("C must reach the value field as a character, got %q", c.EditValueForTest())
	}
}
