package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/config"
)

// ApplyAppearance runs before Bubble Tea owns input. termenv queries OSC 11,
// falls back to COLORFGBG, then dark. A fresh output keeps System independent
// of a previous explicit Lip Gloss override. Forced modes never probe the TTY.
func ApplyAppearance(choice string) {
	applyAppearance(choice, func() bool {
		return termenv.NewOutput(lipgloss.DefaultRenderer().Output().Writer()).HasDarkBackground()
	})
}

func applyAppearance(choice string, detectDark func() bool) {
	dark := true
	switch config.NormalizeAppearance(choice) {
	case "light":
		dark = false
	case "system":
		dark = detectDark()
	}
	lipgloss.SetHasDarkBackground(dark)
}

// Older daemon manifests can still advertise retired palette keys. Keep them
// out of the TUI while the daemon operation is retired separately in #3936.
func visibleAppearanceEntries(entries []config.ConfigEntry) []config.ConfigEntry {
	out := make([]config.ConfigEntry, 0, len(entries))
	for _, e := range entries {
		if e.Key != "theme" && !strings.HasPrefix(e.Key, "theme.") {
			out = append(out, e)
		}
	}
	return out
}

func appearanceLabel(value string) string {
	switch config.NormalizeAppearance(value) {
	case "light":
		return "Light"
	case "dark":
		return "Dark"
	default:
		return "System"
	}
}
func configEnumLabels(e config.ConfigEntry) string {
	if e.Key == "appearance" {
		return "Light · Dark · System"
	}
	return strings.Join(e.Enum, " · ")
}
