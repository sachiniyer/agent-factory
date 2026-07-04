package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui/tree"
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

// TestAutomationTitleMatchesInstanceTitle pins #1126: an automation's title
// renders in the exact color the instances tree uses for instance titles, so
// the two stacked lists read as one. Both must resolve to the shared
// tree.InstanceTitleColor — a future literal can't silently drift them apart.
func TestAutomationTitleMatchesInstanceTitle(t *testing.T) {
	if got := automationItemTitleStyle.GetForeground(); got != lipgloss.TerminalColor(tree.InstanceTitleColor) {
		t.Errorf("automation title foreground = %v, want tree.InstanceTitleColor (%v)",
			got, tree.InstanceTitleColor)
	}
}
