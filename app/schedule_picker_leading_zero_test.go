package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

func TestTaskEditorRendersLeadingZeroEveryNHoursAsPreset(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{{width: 80, height: 24}, {width: 72, height: 20}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			h := newTestHome(t)
			tasks := []task.Task{{
				ID:       "leading-zero-hours",
				Name:     "poller",
				CronExpr: "00 */2 * * *",
				Enabled:  true,
			}}
			h.store.SetTasks(tasks)
			h.automations.TaskPane().SetTasks(tasks)
			resizeHome(h, size.width, size.height)
			h.focusRegion(layout.RegionAutomations)

			_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyEnter})
			require.True(t, consumed)
			require.True(t, h.automations.TaskPane().IsEditing())

			frame := h.View()
			requireViewSized(t, frame, size.width, size.height)
			require.Contains(t, frame, "Every 2 hours",
				"the production task editor must seed the semantic preset")
			require.NotContains(t, frame, "Custom: 00 */2 * * *",
				"the equivalent cron must not fall back to the raw editor")
		})
	}
}
