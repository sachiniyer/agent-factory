package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// TestAutomationsRegistersRowZones (#1024 R4): the rail section registers its
// rect plus one row zone per rendered task, each sitting on the line that
// renders that task; rows win the overlap, the title line falls through to
// the section background.
func TestAutomationsRegistersRowZones(t *testing.T) {
	tasks := stripTasks()
	a := newTestAutomations(tasks)
	reg := zones.NewRegistry()
	a.SetZoneRegistry(reg)
	rect := layout.Rect{X: 2, Y: 40, W: 90, H: 3}
	a.SetRect(rect)

	reg.Reset()
	lines := plainLines(a.String())

	bg, ok := reg.Find(zones.AutoBG)
	require.True(t, ok)
	assert.Equal(t, rect, bg)

	for _, tsk := range tasks {
		r, ok := reg.Find(zones.AutoTask(tsk.ID))
		require.True(t, ok, "row zone for %s; got %v", tsk.Name, reg.IDs())
		assert.Equal(t, 1, r.H)
		assert.Contains(t, lines[r.Y-rect.Y], tsk.Name,
			"the zone must sit on the row rendering its task")
	}

	// Task rows win over the section background where they overlap.
	r, _ := reg.Find(zones.AutoTask(tasks[0].ID))
	id, _, ok := reg.Resolve(r.X+1, r.Y)
	require.True(t, ok)
	assert.Equal(t, zones.AutoTask(tasks[0].ID), id)
	// The title line above them falls through to the background.
	id, _, ok = reg.Resolve(rect.X+1, rect.Y)
	require.True(t, ok)
	assert.Equal(t, zones.AutoBG, id)
}

// TestAutomationsCompactSummaryRegistersBGOnly: the 1-line degraded summary
// has no task rows, so only the background zone exists.
func TestAutomationsCompactSummaryRegistersBGOnly(t *testing.T) {
	a := newTestAutomations(stripTasks())
	reg := zones.NewRegistry()
	a.SetZoneRegistry(reg)
	a.SetRect(layout.Rect{X: 0, Y: 40, W: 70, H: 1})
	a.SetCompact(true)

	reg.Reset()
	_ = a.String()
	assert.Equal(t, []string{zones.AutoBG}, reg.IDs(),
		"the compact summary registers only the section background")
}

// TestAutomationsScrolledWindowRegistersVisibleRowsOnly: rows scrolled out of
// the section's window register no zones, so a click can never land on a task
// that isn't on screen.
func TestAutomationsScrolledWindowRegistersVisibleRowsOnly(t *testing.T) {
	tasks := stripTasks()
	extra := stripTasks()
	extra[0].ID, extra[0].Name = "3", "third-task"
	extra[1].ID, extra[1].Name = "4", "fourth-task"
	tasks = append(tasks, extra...)
	a := newTestAutomations(tasks)
	reg := zones.NewRegistry()
	a.SetZoneRegistry(reg)
	a.SetRect(layout.Rect{X: 0, Y: 30, W: 90, H: 3}) // title + 2 rows for 4 tasks
	a.Focus()
	// Walk the cursor to the last task so the window slides past the first.
	for range tasks {
		a.ScrollDown()
	}

	reg.Reset()
	_ = a.String()

	_, first := reg.Find(zones.AutoTask("1"))
	assert.False(t, first, "rows scrolled above the window must not register zones")
	r, ok := reg.Find(zones.AutoTask("4"))
	require.True(t, ok, "the cursor's row is inside the window; got %v", reg.IDs())
	assert.LessOrEqual(t, r.Bottom(), a.rect.Bottom())
}

// TestAutomationsSelectTaskByID covers the click primitive the mouse router
// calls: the section cursor lands on the clicked task.
func TestAutomationsSelectTaskByID(t *testing.T) {
	tasks := stripTasks()
	a := newTestAutomations(tasks)

	require.True(t, a.SelectTaskByID(tasks[1].ID))
	assert.Equal(t, 1, a.SelectedTaskIndex(), "the click selects the row's task")
	assert.False(t, a.SelectTaskByID("nope"), "unknown ids are refused")
	assert.Equal(t, 1, a.SelectedTaskIndex(), "a refused click must not move the cursor")
}
