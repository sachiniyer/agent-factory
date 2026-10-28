package doctor

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticStatusesReserveLivenessColors(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		for _, status := range []CheckStatus{StatusPass, StatusFixed, StatusWarn, StatusFail} {
			role := theme.Roles().Ink
			if status == StatusFail {
				role = theme.Roles().Dead
			}
			plain := fmt.Sprintf("%-5s", status)
			require.Equal(t, lipgloss.NewStyle().Foreground(role).Render(plain), renderStatus(status, true))
			require.Equal(t, plain, renderStatus(status, false))
		}
	}
}
