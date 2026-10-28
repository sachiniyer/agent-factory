package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

func TestDialogBackgroundSurvivesNestedTextStyles(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	for _, mode := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(mode)
		content := DialogTitleStyle().Render("label") + " after"
		got := RenderDialog(DialogStyle(), content)
		// Interpret the background at the unstyled text after the nested reset.
		prefix, _, found := strings.Cut(got, "after")
		require.True(t, found)
		background := ""
		for _, sgr := range regexp.MustCompile(`\x1b\[([0-9;]*)m`).FindAllStringSubmatch(prefix, -1) {
			params := strings.Split(sgr[1], ";")
			for i := 0; i < len(params); i++ {
				switch params[i] {
				case "0", "", "49":
					background = ""
				case "38", "48":
					if i+4 < len(params) && params[i+1] == "2" {
						if params[i] == "48" {
							background = strings.Join(params[i:i+5], ";")
						}
						i += 4
					}
				}
			}
		}
		want := termenv.TrueColor.FromColor(CurrentTheme().SurfaceRaised).Sequence(true)
		require.Equal(t, want, background, "nested reset must not expose the base surface")
	}
}
