package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestSelectionMarkerUsesAccentOnRaised(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		roles := CurrentTheme()
		marker := lipgloss.NewStyle().Bold(true).Foreground(roles.Accent).Background(roles.SurfaceRaised).Render("▸ ")
		hooks := NewHooksPane()
		hooks.SetCommands([]string{"make test"})
		hooks.SetSize(60, 12)
		hooks.SetFocus(true)
		require.Contains(t, hooks.String(), marker)
		tasks := NewTaskPane()
		tasks.SetTasks([]task.Task{{ID: "one", Name: "Review"}})
		tasks.SetSize(60, 12)
		tasks.SetFocus(true)
		require.Contains(t, tasks.String(), marker)
	}
}
