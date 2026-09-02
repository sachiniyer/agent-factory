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
	assert.Contains(t, out, "overdue · missed 18 · 0 3 * * *",
		"the warning leads the detail line, ahead of the task's own configuration, "+
			"because a narrow rail ellipsizes from the right:\n%s", out)
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

// TestAutomationsOverdueSurvivesTheNarrowRail: the rail's minimum is 22 columns
// and the detail line is ellipsized from the right, so ordering IS the design
// here — a warning that only fits at 100 columns is not a warning on the rail
// this pane actually lives in.
func TestAutomationsOverdueSurvivesTheNarrowRail(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask()})
	a.SetRect(layout.Rect{W: 30, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "overdue",
		"the warning must survive the ellipsis at the rail's narrow end:\n%s", out)
}

// TestAutomationsCompactSummaryKeepsTheCountAtTheRailMinimum: the compact mode
// has no rows, so this one line is the entire section — and it is also the
// narrowest thing the rail ever draws. At 22 columns the section label itself
// does not fit; the count has to survive where it does not, or the degraded rail
// shows no warning at all.
func TestAutomationsCompactSummaryKeepsTheCountAtTheRailMinimum(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask(), stripTasks()[1]})
	a.SetRect(layout.Rect{W: 22, H: 1})
	a.SetCompact(true)

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 1}, "compact strip at the rail minimum")
	assert.Contains(t, out, "1 overdue", "the count outranks the section label here:\n%s", out)
	assert.Contains(t, out, "manage", "and the manager key is still the last thing cut")
}

// TestAutomationsMarksACappedMissedCount: the derivation saturates its walk, so
// a task dark for long enough reports a FLOOR. Rendering it as a bare "10000"
// would state an exact number the derivation never computed.
func TestAutomationsMarksACappedMissedCount(t *testing.T) {
	capped := overdueStripTask()
	capped.MissedOccurrences, capped.MissedOccurrencesCapped = task.MaxMissedOccurrences, true

	a := newTestAutomations([]task.Task{capped})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "overdue · missed 10000+",
		"a saturated count is a lower bound and must read as one:\n%s", out)
}

// TestAutomationsMarksATaskThatCanNeverFire: the rail reads the RECORD, so a
// verdict that lives only in a recomputed ScheduleHealth is one it cannot see —
// and it would render an enabled task with an impossible expression exactly like
// a healthy one.
func TestAutomationsMarksATaskThatCanNeverFire(t *testing.T) {
	feb31 := stripTasks()[0]
	feb31.CronExpr, feb31.Unschedulable = "0 0 31 2 *", true

	a := newTestAutomations([]task.Task{feb31})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "▾[!]  nightly-sweep",
		"a task that can never fire carries the same warning glyph as one that stopped:\n%s", out)
	assert.Contains(t, out, "No upcoming run",
		"and the detail line keeps #2596's wording rather than repeating itself:\n%s", out)
	assert.NotContains(t, out, "overdue", "it was never due, so it is not late")
}

// TestAutomationsCompactSummaryCountsTasksThatCanNeverFire: the degraded
// one-liner counts every task needing attention, not only the overdue ones.
func TestAutomationsCompactSummaryCountsTasksThatCanNeverFire(t *testing.T) {
	feb31 := stripTasks()[0]
	feb31.Unschedulable = true

	a := newTestAutomations([]task.Task{feb31, overdueStripTask()})
	a.SetRect(layout.Rect{W: 70, H: 1})
	a.SetCompact(true)

	assert.Contains(t, a.View(), "2 overdue")
}

// TestAutomationsDiagnosesAMalformedExpression: an expression that does not
// parse is the one unschedulable shape with no other explanation on the detail
// line — "No upcoming run" is emitted only after a successful parse — so without
// its own fragment the row showed a raw expression and a [!] with nothing saying
// why.
func TestAutomationsDiagnosesAMalformedExpression(t *testing.T) {
	broken := stripTasks()[0]
	broken.CronExpr, broken.Unschedulable = "99 * * * *", true

	a := newTestAutomations([]task.Task{broken})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "▾[!]  nightly-sweep")
	assert.Contains(t, out, "Invalid cron expression",
		"the mark needs a reason on the line beneath it:\n%s", out)
	assert.NotContains(t, out, "next ", "and no fire time is promised")
}
