package overlay

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/require"
)

func TestPickerMarkersUseAccent(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		marker := ui.SelectionMarker("▸ ")
		require.Contains(t, NewSelectionOverlay("Select", []string{"First"}).Render(), marker)
		require.Contains(t, NewProjectPickerOverlay([]Project{{Name: "First", Root: "/first"}}, "/first").Render(), marker)
		require.Contains(t, NewProjectPickerOverlay(nil, "").Render(), marker)
	}
}
