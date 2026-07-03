package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// zoneTestTasks builds a projection-backed automations pane with two tasks.
func zoneTestTasks(t *testing.T) (*AutomationsPane, *zones.Registry, []task.Task) {
	t.Helper()
	tasks := []task.Task{
		{ID: "task-aaa", Name: "nightly-sweep", CronExpr: "0 3 * * *", Enabled: true},
		{ID: "task-bbb", Name: "log-watch", WatchCmd: "tail -f x", Enabled: false},
	}
	proj := store.NewProjection()
	proj.SetTasks(tasks)
	a := NewAutomationsPane(proj)
	a.TaskPane().SetTasks(tasks)
	a.now = func() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }
	reg := zones.NewRegistry()
	a.SetZoneRegistry(reg)
	return a, reg, tasks
}

// TestAutomationsRegistersCompactRowZones (#1024 PR 6): the unfocused strip
// registers its rect plus one row zone per rendered task, each sitting on the
// line that renders that task.
func TestAutomationsRegistersCompactRowZones(t *testing.T) {
	a, reg, tasks := zoneTestTasks(t)
	rect := layout.Rect{X: 0, Y: 40, W: 100, H: 3}
	a.SetRect(rect)

	reg.Reset()
	lines := plainLines(a.String())

	strip, ok := reg.Find(zones.AutoStrip)
	require.True(t, ok)
	assert.Equal(t, rect, strip)

	for _, tsk := range tasks {
		r, ok := reg.Find(zones.AutoTask(tsk.ID))
		require.True(t, ok, "row zone for %s; got %v", tsk.Name, reg.IDs())
		assert.Equal(t, 1, r.H)
		assert.Contains(t, lines[r.Y-rect.Y], tsk.Name,
			"the zone must sit on the row rendering its task")
	}

	// Task rows win over the strip background where they overlap.
	r, _ := reg.Find(zones.AutoTask("task-aaa"))
	id, _, ok := reg.Resolve(r.X+1, r.Y)
	require.True(t, ok)
	assert.Equal(t, zones.AutoTask("task-aaa"), id)
	// The title line above them falls through to the strip.
	id, _, ok = reg.Resolve(rect.X+1, rect.Y)
	require.True(t, ok)
	assert.Equal(t, zones.AutoStrip, id)
}

// TestAutomationsRegistersExpandedRowZones: focused, the strip hosts the full
// task manager and the row zones follow the manager's real row spans —
// including the selected row's multi-line detail block.
func TestAutomationsRegistersExpandedRowZones(t *testing.T) {
	a, reg, tasks := zoneTestTasks(t)
	rect := layout.Rect{X: 0, Y: 24, W: 100, H: 12}
	a.SetRect(rect)
	a.Focus()

	reg.Reset()
	lines := plainLines(a.String())

	for _, tsk := range tasks {
		r, ok := reg.Find(zones.AutoTask(tsk.ID))
		require.True(t, ok, "expanded row zone for %s", tsk.Name)
		assert.Contains(t, lines[r.Y-rect.Y], tsk.Name,
			"the expanded zone must start on the row rendering its task")
	}
	// The selected task's block spans its detail lines.
	sel, ok := a.TaskPane().SelectedTask()
	require.True(t, ok)
	r, _ := reg.Find(zones.AutoTask(sel.ID))
	assert.Greater(t, r.H, 1, "the selected row's zone covers its detail lines")
}

// TestAutomationsCompactSummaryRegistersStripOnly: the 1-line degraded strip
// has no task rows, so only the strip zone exists.
func TestAutomationsCompactSummaryRegistersStripOnly(t *testing.T) {
	a, reg, _ := zoneTestTasks(t)
	rect := layout.Rect{X: 0, Y: 40, W: 70, H: 1}
	a.SetRect(rect)
	a.SetCompact(true)

	reg.Reset()
	_ = a.String()
	assert.Equal(t, []string{zones.AutoStrip}, reg.IDs(),
		"the compact summary registers only the strip zone")
}

// TestAutomationsEditFormRegistersNoRowZones: while the edit form owns the
// expanded strip there are no task rows on screen, so a stale click can't
// re-select a task behind the form.
func TestAutomationsEditFormRegistersNoRowZones(t *testing.T) {
	a, reg, _ := zoneTestTasks(t)
	a.SetRect(layout.Rect{X: 0, Y: 24, W: 100, H: 12})
	a.Focus()
	a.TaskPane().StartEditSelected()
	require.True(t, a.TaskPane().IsEditing())

	reg.Reset()
	_ = a.String()
	for _, id := range reg.IDs() {
		_, isTask := zones.AutoTaskID(id)
		assert.False(t, isTask, "no task row zones while the edit form is up (got %s)", id)
	}
}

// TestTaskPaneClickPrimitives covers the click actions the mouse router
// calls: SelectTaskByID and the double-click editor open.
func TestTaskPaneClickPrimitives(t *testing.T) {
	a, _, tasks := zoneTestTasks(t)
	sp := a.TaskPane()

	require.True(t, sp.SelectTaskByID(tasks[1].ID))
	sel, ok := sp.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, tasks[1].ID, sel.ID)

	assert.False(t, sp.SelectTaskByID("nope"), "unknown ids are refused")

	sp.StartEditSelected()
	assert.True(t, sp.IsEditing(), "double-click opens the editor for the selection")
	assert.False(t, sp.SelectTaskByID(tasks[0].ID),
		"selection is pinned while the edit form owns the pane")
}
