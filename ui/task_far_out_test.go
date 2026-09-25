package ui

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
)

// #4843: a dated cron re-armed for next year must not render as the yearless
// "next Sep 21 07:00" in either task list.

func farOutRailTask(next time.Time) task.Task {
	return task.Task{
		ID: "f77f4e99", Name: "refresh-reading-plan", CronExpr: "0 7 21 9 *", Enabled: true,
		Arming: task.ArmingArmed, NextRunAt: &next,
	}
}

func TestAutomationsFlagsAFarOutNextRun(t *testing.T) {
	// newTestAutomations pins now at 2026-07-02 02:00 UTC.
	far := farOutRailTask(time.Date(2027, time.September, 21, 7, 0, 0, 0, time.UTC))
	a := newTestAutomations([]task.Task{far})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "next 2027-09-21 (in 14 months)", out)
	assert.NotContains(t, out, "Sep 21 07:00")

	near := farOutRailTask(time.Date(2026, time.August, 1, 7, 0, 0, 0, time.UTC))
	a = newTestAutomations([]task.Task{near})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()
	assert.Contains(t, a.View(), "next Aug 01 07:00", "a near run keeps the short form")
}

func TestTaskPaneFlagsAFarOutNextRun(t *testing.T) {
	pane := NewTaskPane()
	pane.setNowForTest(func() time.Time { return time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC) })
	pane.SetSize(100, 12)
	far := farOutRailTask(time.Date(2027, time.September, 21, 7, 0, 0, 0, time.UTC))
	near := farOutRailTask(time.Date(2026, time.October, 1, 7, 0, 0, 0, time.UTC))
	near.ID, near.Name = "n", "weekly-sweep"
	pane.SetTasks([]task.Task{far, near})

	out := pane.String()
	assert.Contains(t, out, "next 2027-09-21 (in 11 months)", out)
	assert.Contains(t, out, "next Oct 01 07:00")
}
