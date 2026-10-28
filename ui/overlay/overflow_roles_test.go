package overlay

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/require"
)

func TestOverflowRowsUseMutedInk(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	var instances []*session.Instance
	var items []string
	var projects []Project
	for i := 0; i < 40; i++ {
		title := fmt.Sprintf("Item %02d", i)
		items = append(items, title)
		instances = append(instances, &session.Instance{Title: title})
		projects = append(projects, Project{Name: title, Root: title})
	}
	for _, dark := range []bool{false, true} {
		lipgloss.SetHasDarkBackground(dark)
		for _, height := range []int{12, 24} {
			search := NewSearchOverlay(instances)
			search.SetMaxSize(80, height)
			search.SetSelectedIndex(20)
			selection := NewSelectionOverlay("Pick", items)
			selection.SetMaxSize(80, height)
			selection.SetSelectedIndex(20)
			picker := NewProjectPickerOverlay(projects, "")
			picker.SetMaxSize(80, height)
			picker.selectedIdx = 20
			for _, frame := range []string{search.Render(), selection.Render(), picker.Render()} {
				above, below := false, false
				for _, row := range strings.Split(frame, "\n") {
					plain := xansi.Strip(row)
					if !strings.Contains(plain, "more above") && !strings.Contains(plain, "more below") {
						continue
					}
					above = above || strings.Contains(plain, "more above")
					below = below || strings.Contains(plain, "more below")
					muted := termenv.TrueColor.FromColor(ui.CurrentTheme().InkMuted).Sequence(false)
					require.Contains(t, row, "\x1b["+muted+"m")
				}
				require.True(t, above)
				require.True(t, below)
			}
		}
	}
}
