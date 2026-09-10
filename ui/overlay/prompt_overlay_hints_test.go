package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

// The naming form's overlay must keep describing the naming form. This is the
// control for the #4172 change: SetHints exists so a caller can override the
// copy, and this pins that not calling it changes nothing.
func TestPromptOverlayKeepsComposerHintsByDefault(t *testing.T) {
	forceProfile(t, termenv.TrueColor)
	p := NewPromptOverlay("Initial prompt", "fix the flaky test")
	p.SetMaxSize(80, 24)
	got := p.Render()

	for _, want := range []string{"enter newline", "tab done", "ctrl+c cancel", "Initial prompt"} {
		if !strings.Contains(got, want) {
			t.Errorf("default prompt overlay lost %q; the naming form's semantics are unchanged by #4172:\n%s", want, got)
		}
	}
}

// #4172: the jump-to-tab prompt re-owns Enter/Esc (handleStateJumpTab) and so
// must describe ITS keys. Before this, it rendered the composer's hint, which
// told the operator enter made a newline while enter actually submitted, and
// advertised a tab key that did nothing.
func TestPromptOverlayCallerCanReplaceHintsAndPlaceholder(t *testing.T) {
	// Pin the profile: the placeholder's styling renders differently under a
	// leaked ANSI256 profile, and a test that depends on run order is not a test.
	forceProfile(t, termenv.TrueColor)
	p := NewPromptOverlay("Jump to tab (number or name)", "")
	p.SetPlaceholder("Tab number or name…")
	p.SetHints("enter jump · esc cancel", "enter jump · esc cancel")
	p.SetMaxSize(80, 24)
	got := p.Render()

	if got := p.textarea.Placeholder; got != "Tab number or name…" {
		t.Errorf("placeholder = %q, want the jump's own; the composer's invites a prompt for the agent", got)
	}
	// The placeholder's RENDERED form depends on the process-wide lipgloss
	// profile, which other tests in this package mutate, so it is asserted on
	// the widget above rather than in the styled output. The hints below are
	// plain text and render identically under every profile.
	for _, absent := range []string{"enter newline", "tab done"} {
		if strings.Contains(got, absent) {
			t.Errorf("jump prompt still renders the composer's copy %q, which is wrong for this overlay:\n%s", absent, got)
		}
	}
	for _, want := range []string{"enter jump", "esc cancel"} {
		if !strings.Contains(got, want) {
			t.Errorf("jump prompt is missing %q:\n%s", want, got)
		}
	}
}
