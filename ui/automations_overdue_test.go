package ui

import (
	"fmt"
	"strings"
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
	assert.Contains(t, out, "Automations: 2 (1 on · [!] 1)",
		"the label is the mark, not the word: the count spans every marked row, and an "+
			"expression the scheduler cannot fire is not late")
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
// narrowest thing the rail ever draws. At 22 columns #3641's ladder has shed the
// noun and the secondary number; the count that says something is WRONG has to
// be what survives that last rung, or the degraded rail shows no warning at all.
//
// It survives as the row glyph rather than the word, and that is a width
// decision rather than a style one — see TestAutomationsCompactPrimaryFitsAtAnyCount.
func TestAutomationsCompactSummaryKeepsTheCountAtTheRailMinimum(t *testing.T) {
	a := newTestAutomations([]task.Task{overdueStripTask(), stripTasks()[1]})
	a.SetRect(layout.Rect{W: 22, H: 1})
	a.SetCompact(true)

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 1}, "compact strip at the rail minimum")
	assert.Contains(t, out, "[!] 1", "the warning count outranks the total here:\n%s", out)
	assert.Contains(t, out, "m manage", "and the manager key is still the last thing cut")
}

// TestAutomationsCompactPrimaryFitsAtAnyCount: #3641's last rung exists to keep
// the manage affordance from being clipped at the 22-column rail minimum, and a
// three-digit count spelled out as "100 overdue" needs 12 of the 11 cells that
// rung has — measured, it fell straight through to the clip and truncated the
// affordance, which is the contract that rung protects. The glyph form fits at
// any count, and a spelling chosen by digit count would be no better: rebinding
// the manager key changes the budget.
func TestAutomationsCompactPrimaryFitsAtAnyCount(t *testing.T) {
	last := time.Date(2026, time.June, 14, 3, 0, 0, 0, time.UTC)
	for _, overdue := range []int{1, 99, 150} {
		var tasks []task.Task
		for i := 0; i < overdue; i++ {
			tasks = append(tasks, task.Task{
				ID: fmt.Sprintf("t%d", i), Name: "x", CronExpr: "0 3 * * *", Enabled: true,
				LastRunAt: &last, Overdue: true, MissedOccurrences: 5,
			})
		}
		a := newTestAutomations(tasks)
		a.SetRect(layout.Rect{W: 22, H: 1})
		a.SetCompact(true)

		out := a.View()
		assert.Contains(t, out, fmt.Sprintf("[!] %d", overdue),
			"the warning count survives at %d overdue:\n%s", overdue, out)
		assert.Contains(t, out, "m manage",
			"and the affordance is still the last thing cut at %d overdue:\n%s", overdue, out)
	}
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

	assert.Contains(t, a.View(), "[!] 2")
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
	assert.Contains(t, out, "Invalid cron expression · 99 * * * *",
		"the reason LEADS the line, ahead of the expression it is about:\n%s", out)
	assert.NotContains(t, out, "next ", "and no fire time is promised")
}

// TestAutomationsDiagnosisSurvivesTheRailMinimum: at 22 columns the detail line
// keeps 16 cells and is clipped from the right, so a reason placed behind the
// expression leaves the mark unexplained — which is the whole failure the mark
// exists to prevent, one level down.
func TestAutomationsDiagnosisSurvivesTheRailMinimum(t *testing.T) {
	broken := stripTasks()[0]
	broken.CronExpr, broken.Unschedulable = "99 * * * *", true

	a := newTestAutomations([]task.Task{broken})
	a.SetRect(layout.Rect{W: 22, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "Invalid cron",
		"enough of the reason has to survive the clip to read as one:\n%s", out)
}

// TestAutomationsMarksAnUnassessableRowUnknown: a record whose health could not
// be established must not keep the tick — that is the claim this whole change
// exists to stop making — and must not carry the warning mark either, which
// would call an unestablished thing a failure. It gets its own.
func TestAutomationsMarksAnUnassessableRowUnknown(t *testing.T) {
	blank := stripTasks()[0]
	blank.Unassessable = true

	a := newTestAutomations([]task.Task{blank})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "▾[?]  nightly-sweep",
		"unknown gets its own mark, not a tick and not a warning:\n%s", out)
	assert.NotContains(t, out, "[✓]")
	assert.NotContains(t, out, "[!]")
	assert.Contains(t, out, "Health unknown", "and the row says what it could not do")
}

// TestAutomationsAttentionCountExcludesUnknowns: the compact summary answers
// "is anything wrong?", and an unknown is not an answer of yes. It is the same
// line doctor draws, and the rows carry "[?]" at any width that has rows.
func TestAutomationsAttentionCountExcludesUnknowns(t *testing.T) {
	blank := stripTasks()[0]
	blank.Unassessable = true

	a := newTestAutomations([]task.Task{blank, stripTasks()[1]})
	a.SetRect(layout.Rect{W: 70, H: 1})
	a.SetCompact(true)

	out := a.View()
	assert.Contains(t, out, "Automations: 2 (1 on)", "no attention count when nothing is wrong")
	assert.NotContains(t, out, "[!]")
}

// TestAutomationsLeadsWithBothUnschedulableDiagnoses: a valid expression with no
// upcoming occurrence took the summary's path, which puts "No upcoming run"
// AFTER the trigger — clipped at the rail minimum to "0 0 31 2 * · …", a [!] row
// with no explanation. That is the exact failure the mark exists to prevent, and
// it is the same ordering rule the invalid case already followed.
func TestAutomationsLeadsWithBothUnschedulableDiagnoses(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"0 0 31 2 *", "No upcoming run"},
		{"99 * * * *", "Invalid cron expression"},
	} {
		tsk := stripTasks()[0]
		tsk.CronExpr, tsk.Unschedulable = tc.expr, true

		a := newTestAutomations([]task.Task{tsk})
		a.SetRect(layout.Rect{W: 100, H: 4})
		a.Focus()
		wide := a.View()
		assert.Contains(t, wide, tc.want+" · "+tc.expr,
			"%q: the reason leads the line:\n%s", tc.expr, wide)
		assert.Equal(t, 1, strings.Count(wide, tc.want),
			"%q: and is said once — the summary must not repeat it", tc.expr)

		a.SetRect(layout.Rect{W: 22, H: 4})
		narrow := a.View()
		assert.Contains(t, narrow, tc.want[:8],
			"%q: enough of it survives the rail minimum to read as a reason:\n%s", tc.expr, narrow)
	}
}

// TestAutomationsDiagnosesEveryUnschedulableShape is the pane's half of the
// consolidation: it renders the SHARED classification instead of re-deriving it,
// which is what had it calling an absent expression invalid. Every shape, at
// full width and at the rail minimum where the reason is all that survives.
func TestAutomationsDiagnosesEveryUnschedulableShape(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"", "No trigger"},
		{"99 * * * *", "Invalid cron expression"},
		{"0 0 31 2 *", "No upcoming run"},
	} {
		tsk := stripTasks()[0]
		tsk.CronExpr, tsk.Unschedulable = tc.expr, true

		a := newTestAutomations([]task.Task{tsk})
		a.SetRect(layout.Rect{W: 100, H: 4})
		a.Focus()
		wide := a.View()
		assert.Contains(t, wide, "▾[!]  nightly-sweep", "%q is marked", tc.expr)
		assert.Contains(t, wide, tc.want, "%q reads %q:\n%s", tc.expr, tc.want, wide)
		assert.Equal(t, 1, strings.Count(wide, tc.want), "%q: said once", tc.expr)

		a.SetRect(layout.Rect{W: 22, H: 4})
		assert.Contains(t, a.View(), tc.want[:8],
			"%q: the reason survives the rail minimum", tc.expr)
	}
}
