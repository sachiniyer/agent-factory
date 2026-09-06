package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

func TestAccentColorValue(t *testing.T) {
	if AccentColor != theme.Colors()["accent"] {
		t.Fatal("accent must use generated role")
	}
}

// Headings and actionable shortcuts use ink; focus does not add a palette.
func TestChromeCopyUsesInk(t *testing.T) {
	for name, got := range map[string]lipgloss.TerminalColor{
		"rail title":        mainTitle.GetForeground(),
		"automations title": automationsTitleStyle.GetForeground(),
		"menu action":       actionGroupStyle.GetForeground(),
	} {
		if got != CurrentTheme().Ink {
			t.Errorf("%s uses %v, want ink", name, got)
		}
	}
}

func TestLegacyPaletteCannotOverrideRoles(t *testing.T) {
	original := CurrentTheme()
	ApplyTheme()
	t.Cleanup(func() { ApplyTheme() })
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
	ApplyTheme()
	roles := theme.Roles()
	if configSelectedStyle.GetForeground() != roles.Ink || configSelectedStyle.GetBackground() != roles.SurfaceRaised {
		t.Fatal("config key and account selections must use ink on surface-raised")
	}
	// Plain ink on surface removes the decorative chip while preserving contrast.
	if blurredTitle.GetForeground() != roles.Ink || blurredTitle.GetBackground() != roles.Surface {
		t.Fatal("blurred sidebar title must use the contrast-checked ink on surface pair")
	}
}

func TestConfigLocationAndPurposeHierarchy(t *testing.T) {
	roles := theme.Roles()
	if configLocationStyle.GetForeground() != roles.InkMuted || configPurposeStyle.GetForeground() != roles.Ink {
		t.Fatal("Config location must be muted without dimming entry purposes")
	}
	if taskPlaceholderStyle.GetFaint() {
		t.Fatal("placeholder contrast must not be reduced below its token role")
	}
}
