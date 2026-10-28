package overlay

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/stretchr/testify/require"
)

func TestSearchScrollIndicatorCannotStealResultZone(t *testing.T) {
	for _, height := range []int{12, 30} {
		var instances []*session.Instance
		for i := 0; i < 40; i++ {
			instances = append(instances, &session.Instance{Title: fmt.Sprintf("session-%02d", i)})
		}
		s := NewSearchOverlay(instances)
		s.SetMaxSize(90, height)
		s.SetSelectedIndex(20)
		plan := s.renderPlan(searchOverlayStyle())
		require.True(t, plan.showAbove)
		title := fmt.Sprintf("… %d more above", plan.startIdx)
		instances[plan.startIdx].Title = title
		reg := zones.NewRegistry()
		s.RegisterZones(reg, layout.Point{})
		lines := strings.Split(s.Render(), "\n")
		var matches []int
		for y, line := range lines {
			if strings.Contains(xansi.Strip(line), title) {
				matches = append(matches, y)
			}
		}
		require.Len(t, matches, 2)
		for i := plan.startIdx; i < plan.endIdx; i++ {
			rect, ok := reg.Find(zones.OverlaySearchRow(i))
			require.True(t, ok)
			require.Equal(t, matches[1]+i-plan.startIdx, rect.Y)
			require.Contains(t, xansi.Strip(lines[rect.Y]), instances[i].Title)
		}
	}
}
