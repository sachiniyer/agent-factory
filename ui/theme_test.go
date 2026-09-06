package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

func TestAccentColorValue(t *testing.T) {
	if AccentColor != theme.Colors()["accent"] {
		t.Fatal("accent must use generated role")
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

func TestLegacyPaletteCannotOverrideRoles(t *testing.T) {
	original := CurrentTheme()
	legacy := config.DefaultThemeConfig()
	legacy.Accent = "ignored"
	legacy.Foreground = "ignored"
	ApplyTheme(legacy)
	t.Cleanup(func() { ApplyTheme(config.DefaultThemeConfig()) })
	if CurrentTheme() != original {
		t.Fatal("legacy palette changed fixed roles")
	}
	if tree.InstanceTitleColor != theme.Colors()["ink"] {
		t.Fatal("tree must use ink")
	}
	if windowStyle.GetBorderBottomForeground() != theme.Colors()["border"] {
		t.Fatal("frame must use border")
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

func TestReviewedSelectionAndBlurredTitleRoles(t *testing.T) {
	ApplyTheme(config.DefaultThemeConfig())
	roles := theme.Roles()
	if configSelectedStyle.GetForeground() != roles.Ink || configSelectedStyle.GetBackground() != roles.SurfaceRaised {
		t.Fatal("config key and account selections must use ink on surface-raised")
	}
	// surface on ink-muted is contrast-checked by designtokens.validate.
	if blurredTitle.GetForeground() != roles.Surface || blurredTitle.GetBackground() != roles.InkMuted {
		t.Fatal("blurred sidebar title must use the contrast-checked surface on ink-muted pair")
	}
}
