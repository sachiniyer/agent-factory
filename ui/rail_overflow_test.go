package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// railOverflowTasks builds n enabled cron tasks (task-0..task-N) for the
// rail overflow tests.
func railOverflowTasks(n int) []task.Task {
	out := make([]task.Task, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, task.Task{
			ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("task-%d", i),
			CronExpr: "0 3 * * *", Enabled: true,
		})
	}
	return out
}

// TestAutomationsCompactSummaryFlagsHiddenRows is #6883's reported state: at
// 60x20 the rail's window ends exactly at the section header — "Automations: 1
// (1 on)" counts a task the strip never draws, and nothing says a row is
// hidden. The sessions rail answers the same situation with a "▼ N more" row;
// the compact strip must too, as its last visible row.
func TestAutomationsCompactSummaryFlagsHiddenRows(t *testing.T) {
	lay := layout.Grid{Automations: 1}.Solve(60, 20)
	require.True(t, lay.AutomationsVisible)
	require.True(t, lay.AutomationsCompact, "60x20 is the compact band (the reported size)")

	a := newTestAutomations([]task.Task{
		{ID: "1", Name: "hello-task", CronExpr: "0 3 * * *", Enabled: true},
	})
	a.SetRect(lay.Automations)
	a.SetCompact(lay.AutomationsCompact)

	lines := plainLines(a.View())
	require.Len(t, lines, lay.Automations.H)
	require.Contains(t, lines[0], "Automations: 1 (1 on)",
		"precondition: the compact summary is the issue's header line")
	assert.Contains(t, lines[1], "▼ 1 more",
		"the summary's hidden task row must be counted by a ▼ N more row — "+
			"the header must never be the last visible row while its children are hidden")
}

// TestAutomationsCompactIndicatorCountsEveryHiddenTask pins N exact: the
// number beside ▼ is the exact count of task rows the strip is hiding.
func TestAutomationsCompactIndicatorCountsEveryHiddenTask(t *testing.T) {
	for n := 1; n <= 3; n++ {
		lay := layout.Grid{Automations: n}.Solve(60, 20)
		a := newTestAutomations(railOverflowTasks(n))
		a.SetRect(lay.Automations)
		a.SetCompact(lay.AutomationsCompact)
		lines := plainLines(a.View())
		require.GreaterOrEqual(t, len(lines), 2, "n=%d", n)
		assert.Containsf(t, lines[1], fmt.Sprintf("▼ %d more", n),
			"n=%d: the indicator counts exactly the hidden task rows", n)
	}
}

// TestAutomationsNoIndicatorWhenEverythingFits is the taller control: when the
// strip's window reaches every task row, no overflow marker may render —
// nothing changes when everything fits.
func TestAutomationsNoIndicatorWhenEverythingFits(t *testing.T) {
	lay := layout.Grid{Automations: 1}.Solve(120, 40)
	require.True(t, lay.AutomationsVisible)
	require.False(t, lay.AutomationsCompact, "120x40 leaves the compact band")

	a := newTestAutomations(railOverflowTasks(1))
	a.SetRect(lay.Automations)
	a.SetCompact(lay.AutomationsCompact)

	out := stripANSI(a.View())
	assert.Contains(t, out, "task-0", "control: the one task row renders")
	assert.NotContains(t, out, "more",
		"no ▲/▼ N more indicator may render when every row fits")
}

// TestAutomationsFullWindowFlagsBothDirections covers the full-mode rail
// windowing: rows scrolled out above the window get a "▲ N more" row, rows
// still below it get "▼ N more", and each N is the exact hidden count.
func TestAutomationsFullWindowFlagsBothDirections(t *testing.T) {
	tasks := railOverflowTasks(8)
	a := newTestAutomations(tasks)
	// Grid-floored section: title + up to 3 content rows + the reserved
	// bottom-margin row.
	a.SetRect(layout.Rect{W: 40, H: 5})
	a.Focus()

	// Top of the list: the focused first row expands to two lines, so the
	// marker budget leaves one task row drawn and seven hidden below.
	lines := plainLines(a.View())
	assert.Contains(t, lines[0], "Automations:")
	assert.Contains(t, lines[len(lines)-2], "▼ 7 more",
		"the tasks the window hides below must be counted exactly:\n%s",
		strings.Join(lines, "\n"))
	assert.NotContains(t, a.View(), "▲", "nothing is hidden above at the top")

	// Scroll the cursor to the last task: the window flips so the hidden
	// rows sit above.
	for i := 0; i < len(tasks); i++ {
		a.ScrollDown()
	}
	lines = plainLines(a.View())
	assert.Contains(t, lines[1], "▲ 7 more",
		"seven tasks above the cursor's window must be counted:\n%s",
		strings.Join(lines, "\n"))
	assert.NotContains(t, a.View(), "▼ ",
		"nothing is hidden below once the last row is in view")
	assert.Contains(t, strings.Join(lines, "\n"), "task-7",
		"the cursor's row must stay inside the window")
}

// TestAutomationsFullWindowIndicatorStaysExactWhileScrolling walks the cursor
// one row at a time: at every stop the indicators count exactly the tasks the
// window hides on that side.
func TestAutomationsFullWindowIndicatorStaysExactWhileScrolling(t *testing.T) {
	tasks := railOverflowTasks(8)
	a := newTestAutomations(tasks)
	a.SetRect(layout.Rect{W: 40, H: 5})
	a.Focus()

	assertHiddenCounts := func(step string) {
		out := stripANSI(a.View())
		up, down := 0, 0
		for _, line := range plainLines(out) {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "▲ %d more", &n); err == nil {
				up = n
			}
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "▼ %d more", &n); err == nil {
				down = n
			}
		}
		drawn := 0
		for i := range tasks {
			if strings.Contains(out, fmt.Sprintf("task-%d", i)) {
				drawn++
			}
		}
		require.Equal(t, len(tasks), up+down+drawn,
			"%s: every task is either drawn or counted by an indicator", step)
	}

	assertHiddenCounts("initial")
	for i := 0; i < len(tasks); i++ {
		a.ScrollDown()
		assertHiddenCounts(fmt.Sprintf("down %d", i))
	}
	for i := 0; i < len(tasks); i++ {
		a.ScrollUp()
		assertHiddenCounts(fmt.Sprintf("up %d", i))
	}
}

// TestProjectsCompactSummaryFlagsHiddenRows mirrors the Automations fix on the
// bottom-most rail section: the Projects compact summary must count the rows
// it hides.
func TestProjectsCompactSummaryFlagsHiddenRows(t *testing.T) {
	lay := layout.Grid{Automations: 0, Projects: 2}.Solve(60, 20)
	require.True(t, lay.ProjectsVisible)
	require.True(t, lay.ProjectsCompact, "60x20 is the compact band")

	p := newTestProjects(nSimpleProjects(2))
	p.SetRect(lay.Projects)
	p.SetCompact(lay.ProjectsCompact)

	lines := plainLines(p.View())
	require.Len(t, lines, lay.Projects.H)
	require.Contains(t, lines[0], "Project")
	assert.Contains(t, lines[1], "▼ 2 more",
		"the summary's hidden project rows must be counted by a ▼ N more row")
}

// TestProjectsFullWindowFlagsBothDirections mirrors the Automations window
// test for the Projects rail in full mode.
func TestProjectsFullWindowFlagsBothDirections(t *testing.T) {
	p := newTestProjects(nSimpleProjects(8))
	p.SetRect(layout.Rect{W: 40, H: 5})
	p.Focus()

	lines := plainLines(p.View())
	assert.Contains(t, lines[len(lines)-2], "▼ 6 more",
		"rows hidden below the window must be counted:\n%s", strings.Join(lines, "\n"))
	assert.NotContains(t, p.View(), "▲", "nothing is hidden above at the top")

	for i := 0; i < 8; i++ {
		p.ScrollDown()
	}
	lines = plainLines(p.View())
	assert.Contains(t, lines[1], "▲ 6 more",
		"rows hidden above the window must be counted:\n%s", strings.Join(lines, "\n"))
	assert.NotContains(t, p.View(), "▼ ",
		"nothing is hidden below once the last row is in view")
}

// TestProjectsNoIndicatorWhenEverythingFits: the taller control on the
// Projects rail — a window that reaches every row draws no marker.
func TestProjectsNoIndicatorWhenEverythingFits(t *testing.T) {
	p := newTestProjects(nSimpleProjects(2))
	p.SetRect(layout.Rect{W: 40, H: 5})

	out := stripANSI(p.View())
	assert.Contains(t, out, "project-0")
	assert.Contains(t, out, "project-1")
	assert.NotContains(t, out, "more",
		"no ▲/▼ N more indicator may render when every row fits")
}
