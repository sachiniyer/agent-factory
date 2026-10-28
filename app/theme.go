package app

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/ui"
)

func applyTheme() {
	ui.ApplyTheme()
	t := ui.CurrentTheme()
	hooksOverlayStyle = ui.DialogStyle()
	splitDividerStyle = lipgloss.NewStyle().
		Foreground(t.Border)
	titleStyle = ui.DialogTitleStyle()
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	keyStyle = lipgloss.NewStyle().Bold(true).Foreground(t.Ink)
	descStyle = lipgloss.NewStyle().Foreground(t.Ink)
}
