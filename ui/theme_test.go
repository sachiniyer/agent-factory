package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestAccentColorValue pins the shared accent to the teal introduced in #932.
// If someone changes the accent, they change it here in one place and this test
// records the intent — the whole point of the constant (#950).
func TestAccentColorValue(t *testing.T) {
	if got := string(AccentColor); got != "#7cb8bb" {
		t.Fatalf("AccentColor = %q, want %q", got, "#7cb8bb")
	}
}

// TestAccentSitesUseConstant guards against the half-finished migration that
// left the sidebar title banner purple (Color("62")) while everything else went
// teal. These are the most prominent accent surfaces; assert they resolve to
// AccentColor so a future literal can't silently drift them apart again.
func TestAccentSitesUseConstant(t *testing.T) {
	cases := []struct {
		name string
		got  lipgloss.TerminalColor
	}{
		{"sidebar title banner background", mainTitle.GetBackground()},
		{"focused window border", windowStyle.GetBorderBottomForeground()},
		{"automations strip title", automationsTitleStyle.GetForeground()},
		{"menu action group", actionGroupStyle.GetForeground()},
	}
	for _, c := range cases {
		if c.got != lipgloss.TerminalColor(AccentColor) {
			t.Errorf("%s = %v, want AccentColor (%s)", c.name, c.got, AccentColor)
		}
	}
}
