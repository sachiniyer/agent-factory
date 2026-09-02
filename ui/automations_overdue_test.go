package ui

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// overdueStripTask is the rail's view of #3623's fixture: the record arrives
// already carrying the derivation (every task read populates it), so the pane's
// job is to make it impossible to miss.
func overdueStripTask() task.Task {
	last := time.Date(2026, time.June, 14, 3, 0, 0, 0, time.UTC)
	return task.Task{
		ID: "1", Name: "nightly-sweep", CronExpr: "0 3 * * *", Enabled: true,
		LastRunAt: &last, LastRunStatus: "started",
		Overdue: true, MissedOccurrences: 18,
	}
}

// TestAutomationsOverdueRowIsMarkedWhenCollapsed is the whole point of putting
// this in the rail. Collapsed rows are title-only since #1126, so a warning that
// only appears on the focused row is a warning nobody sees — the mark has to be
// on the row you are not looking at.
func TestAutomationsOverdueRowIsMarkedWhenCollapsed(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask()})
	a.SetRect(layout.Rect{W: 100, H: 3})

	out := a.View()
	assert.Contains(t, out, "[!]  nightly-sweep",
		"an overdue task carries a static warning glyph in place of the enabled tick:\n%s", out)
	assert.NotContains(t, out, "[✓]  nightly-sweep",
		"a task that has stopped firing must not keep reading as healthy")
}

// TestAutomationsHealthyRowKeepsItsTick guards the other direction: the mark
// means something only if the ordinary case does not carry it.
func TestAutomationsHealthyRowKeepsItsTick(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 3})

	out := a.View()
	assert.Contains(t, out, "[✓]  nightly-sweep")
	assert.NotContains(t, out, "[!]")
}

// TestAutomationsExpandedRowExplainsOverdue: the glyph says something is wrong,
// and the detail line says what — in the house copy conventions (sentence case,
// " · " between fragments, a static glyph and no animation).
func TestAutomationsExpandedRowExplainsOverdue(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask()})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	require.Contains(t, out, "▾[!]  nightly-sweep")
	assert.Contains(t, out, "last Jun 14 03:00 · overdue · missed 18",
		"the detail names the silence and its size:\n%s", out)
}

// TestAutomationsCompactSummaryCountsOverdue: the one-line degraded mode is
// where a silent task is easiest to miss, so the count rides the summary rather
// than needing a wider terminal.
func TestAutomationsCompactSummaryCountsOverdue(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask(), stripTasks()[1]})
	a.SetRect(layout.Rect{W: 70, H: 1})
	a.SetCompact(true)

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 70, H: 1}, "compact strip")
	assert.Contains(t, out, "Automations: 2 (1 on · 1 overdue)")
}

// TestAutomationsPrefersTheLiveNextRun: when the record carries what the
// scheduler actually has armed, the row shows THAT rather than re-evaluating the
// expression. Recomputing for display is what let the pane render a confident
// "next" for a task the daemon was not holding at all (#3623).
func TestAutomationsPrefersTheLiveNextRun(t *testing.T) {
	armed := stripTasks()[0]
	live := time.Date(2026, time.July, 2, 9, 30, 0, 0, time.UTC)
	armed.Arming, armed.NextRunAt = task.ArmingArmed, &live

	a := newTestAutomations([]task.Task{armed})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "next Jul 02 09:30", "the armed entry's own time")
	assert.NotContains(t, out, "next Jul 02 03:00",
		"the expression's answer must not override what is actually armed:\n%s", out)
}

// TestAutomationsSaysWhenATaskIsNotArmed: an enabled task the daemon is not
// holding will not fire at all, and promising it a next run would be a lie the
// expression is happy to tell.
func TestAutomationsSaysWhenATaskIsNotArmed(t *testing.T) {
	unarmed := stripTasks()[0]
	unarmed.Arming = task.ArmingNotArmed

	a := newTestAutomations([]task.Task{unarmed})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "not armed")
	assert.NotContains(t, out, "next Jul 02 03:00")
}
