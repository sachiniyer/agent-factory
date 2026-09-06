package app

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui"
)

func applyTheme(cfg config.ThemeConfig) {
	ui.ApplyTheme(cfg)
	t := ui.CurrentTheme()
	hooksOverlayStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Accent).
		Padding(1, 2)
	splitDividerStyle = lipgloss.NewStyle().
		Foreground(t.Border)
	titleStyle = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(t.Accent)
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	keyStyle = lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	descStyle = lipgloss.NewStyle().Foreground(t.Ink)
}
