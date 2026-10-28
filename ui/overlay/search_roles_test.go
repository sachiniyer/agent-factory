package overlay

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/theme"
	"github.com/stretchr/testify/require"
)

func TestSearchLivenessRoles(t *testing.T) {
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	lives := map[string]session.Liveness{"running": session.LiveRunning, "ready": session.LiveReady, "lost": session.LiveLost, "dead": session.LiveDead, "archived": session.LiveArchived, "limit-reached": session.LiveLimitReached}
	states := append(theme.States(), theme.State{Name: "unset"})
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.Ascii} {
		lipgloss.SetColorProfile(profile)
		for _, mode := range []bool{false, true} {
			lipgloss.SetHasDarkBackground(mode)
			for _, state := range states {
				for _, op := range []session.InFlightOp{session.OpNone, session.OpCreating, session.OpRestoring, session.OpKilling, session.OpArchiving} {
					inst := &session.Instance{Title: "Result"}
					require.NoError(t, inst.Transition(session.ObserveLiveness(lives[state.Name])))
					inst.SetInFlightOpForTest(op)
					s := NewSearchOverlay([]*session.Instance{inst})
					for _, selected := range []int{0, -1} {
						s.selectedIdx = selected
						reg := zones.NewRegistry()
						s.RegisterZones(reg, layout.Point{})
						rect, ok := reg.Find(zones.OverlaySearchRow(0))
						require.True(t, ok, "state=%s op=%v selected=%v", state.Name, op, selected)
						row := strings.Split(s.Render(), "\n")[rect.Y]
						glyph := state.Glyph
						if op != session.OpNone {
							glyph = ""
						}
						plain := xansi.Strip(row)
						if selected == 0 {
							require.Contains(t, row, ui.SelectionMarker("▸ "))
						}
						if glyph == "" {
							require.NotContains(t, plain, "●")
							require.NotContains(t, plain, "○")
							require.NotContains(t, plain, "◌")
							require.NotContains(t, plain, "▧")
							require.NotContains(t, plain, "◆")
						} else {
							statusStyle := lipgloss.NewStyle().Foreground(state.Color)
							if selected == 0 {
								statusStyle = statusStyle.Background(ui.CurrentTheme().SurfaceRaised)
							}
							require.Contains(t, row, statusStyle.Render(glyph+" "))
						}
						require.Contains(t, plain, "Result")
					}
				}
			}
		}
	}
}
