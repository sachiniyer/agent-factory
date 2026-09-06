package overlay

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/stretchr/testify/require"
)

func TestSearchLivenessRoles(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	roles := theme.Roles()
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		for _, tc := range []struct {
			status session.Status
			role   lipgloss.AdaptiveColor
		}{
			{session.Lost, roles.Lost}, {session.Dead, roles.Dead}, {session.Archived, roles.Archived},
		} {
			inst := &session.Instance{Title: "Result"}
			inst.SetStatusForTest(tc.status)
			rendered := NewSearchOverlay([]*session.Instance{inst}).Render()
			require.Contains(t, rendered, lipgloss.NewStyle().Foreground(tc.role).Render("○"))
		}
	}
}
