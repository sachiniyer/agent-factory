package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

func TestFocusedWeekdayRetainsCheckedState(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		lipgloss.SetColorProfile(profile)
		for _, dark := range []bool{false, true} {
			lipgloss.SetHasDarkBackground(dark)
			p := newSchedulePicker()
			p.setFocused(true)
			p.handleKey(keyType(tea.KeyRight))
			for n := 0; n < 4; n++ {
				p.handleKey(keyType(tea.KeyDown))
			}
			require.Equal(t, cellWeekdays, p.activeCell())
			checked := p.renderWeekdayRow()
			require.Contains(t, xansi.Strip(checked), "[M]")
			p.handleKey(keyType(tea.KeySpace))
			unchecked := p.renderWeekdayRow()
			require.Contains(t, xansi.Strip(unchecked), " M ")
			require.NotContains(t, xansi.Strip(unchecked), "[M]")
			require.Equal(t, lipgloss.Width(checked), lipgloss.Width(unchecked))
			if profile == termenv.TrueColor {
				roles := CurrentTheme()
				focus := lipgloss.NewStyle().Foreground(roles.Ink).Background(roles.SurfaceRaised).Bold(true).Underline(true)
				require.Contains(t, checked, focus.Render("[M]"))
				require.Contains(t, unchecked, focus.Render(" M "))
			}
		}
	}
}
