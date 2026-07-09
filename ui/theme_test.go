package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// TestAccentColorValue pins the default shared accent to the Zenburn blue
// approved in #1389. Custom configs can override it through [theme].accent.
func TestAccentColorValue(t *testing.T) {
	if got := string(AccentColor); got != "#8CD0D3" {
		t.Fatalf("AccentColor = %q, want %q", got, "#8CD0D3")
	}
}

// TestAccentSitesUseConstant guards against the half-finished migration that
// left accent surfaces on unrelated literals while the rest of the TUI used
// the configured accent. These are the most prominent accent surfaces.
// Pane borders are semantic state colors and are covered in tabbed-window
// tests.
func TestAccentSitesUseConstant(t *testing.T) {
	cases := []struct {
		name string
		got  lipgloss.TerminalColor
	}{
		{"sidebar title banner background", mainTitle.GetBackground()},
		{"automations strip title", automationsTitleStyle.GetForeground()},
		{"menu action group", actionGroupStyle.GetForeground()},
	}
	for _, c := range cases {
		if c.got != lipgloss.TerminalColor(AccentColor) {
			t.Errorf("%s = %v, want AccentColor (%s)", c.name, c.got, AccentColor)
		}
	}
}

func TestApplyThemeRebuildsProminentStyles(t *testing.T) {
	defaultTheme := config.DefaultThemeConfig()
	t.Cleanup(func() { ApplyTheme(defaultTheme) })

	custom := defaultTheme
	custom.Accent = "#112233"
	custom.Foreground = "#445566"
	custom.ForegroundMuted = "#556677"
	custom.SelectionBackground = "#223344"
	custom.SelectionForeground = "#DDEEFF"
	custom.PaneBorderDefault = "#778899"
	ApplyTheme(custom)

	assertColor := func(name string, got lipgloss.TerminalColor, want string) {
		t.Helper()
		if got != lipgloss.TerminalColor(lipgloss.Color(want)) {
			t.Fatalf("%s = %v, want %s", name, got, want)
		}
	}
	assertColor("AccentColor", lipgloss.TerminalColor(AccentColor), "#112233")
	assertColor("mainTitle background", mainTitle.GetBackground(), "#112233")
	assertColor("window border", windowStyle.GetBorderBottomForeground(), "#778899")
	assertColor("menu action group", actionGroupStyle.GetForeground(), "#112233")
	assertColor("tree title", lipgloss.TerminalColor(tree.InstanceTitleColor), "#445566")
	assertColor("pane focused header background", paneHeaderFocusedStyle.GetBackground(), "#223344")
	assertColor("pane focused header foreground", paneHeaderFocusedStyle.GetForeground(), "#DDEEFF")
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
