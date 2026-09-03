package api

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
)

// showFixture is #3623's own task, as the record reaches a read surface: the
// derivation is already on it, and the audit trail records the toggle that
// (probably) caused the silence.
func showFixture() task.Task {
	last := time.Date(2026, time.August, 14, 14, 20, 8, 0, time.Local)
	return task.Task{
		ID:                "4ab7ba4f",
		Name:              "Master Health Watch",
		CronExpr:          "20 * * * *",
		Prompt:            "sweep",
		ProjectPath:       "/home/user/claude-squad",
		Program:           "claude",
		Enabled:           true,
		CreatedAt:         time.Date(2026, time.July, 1, 9, 0, 0, 0, time.Local),
		LastRunAt:         &last,
		LastRunStatus:     "started",
		Overdue:           true,
		MissedOccurrences: 3,
		Arming:            task.ArmingNotArmed,
		Audit: []task.AuditEntry{
			{At: time.Date(2026, time.August, 14, 14, 30, 0, 0, time.Local), Actor: task.ActorCLI, Action: task.AuditDisabled, Fields: []string{"enabled"}},
			{At: time.Date(2026, time.September, 1, 14, 5, 0, 0, time.Local), Actor: task.ActorTUI, Action: task.AuditEnabled, Fields: []string{"enabled"}},
		},
	}
}

// TestTasksShow_ReportsTheSilenceAndWhoCausedIt is the command written for the
// question #3623 could not answer. Both halves must be on the page: the machine
// doing the subtraction, and the record of who last touched the switch.
func TestTasksShow_ReportsTheSilenceAndWhoCausedIt(t *testing.T) {
	var out bytes.Buffer
	// Two and a half hours after the re-enable, so the task is late again on its
	// own terms rather than on the strength of the pause it was deliberately in.
	renderTaskShow(&out, showFixture(), time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "Master Health Watch · 4ab7ba4f")
	assert.Contains(t, got, "cron · 20 * * * *")
	assert.Contains(t, got, "2026-08-14 14:20 · started")
	assert.Contains(t, got, "overdue · missed 3 · oldest missed 2026-09-01 14:20",
		"the silence is counted from the re-enable in the trail below, not from a run "+
			"18 days before it — otherwise the row would claim 432 misses the operator "+
			"caused on purpose:\n%s", got)
	assert.Contains(t, got, "enabled but not armed",
		"an enabled task the daemon is not holding will not fire, and must say so:\n%s", got)
	assert.Contains(t, got, "2026-08-14 14:30  cli            disabled · enabled")
	assert.Contains(t, got, "2026-09-01 14:05  tui            enabled · enabled")
}

// TestTasksShow_UnknownArmingIsNotReportedAsUnarmed: with no daemon reachable
// nothing observed the arming state, and "unknown" must not read as "broken".
func TestTasksShow_UnknownArmingIsNotReportedAsUnarmed(t *testing.T) {
	tsk := showFixture()
	tsk.Arming = task.ArmingUnknown

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))

	assert.Contains(t, out.String(), "unknown — nothing has reported on it yet")
	assert.NotContains(t, out.String(), "not armed")
}

// TestTasksShow_HealthyTaskSaysSo keeps the report readable in the ordinary
// case; a page that only ever says "overdue" teaches nobody what normal is.
func TestTasksShow_HealthyTaskSaysSo(t *testing.T) {
	tsk := showFixture()
	last := time.Date(2026, time.September, 1, 14, 20, 5, 0, time.Local)
	next := time.Date(2026, time.September, 1, 15, 20, 0, 0, time.Local)
	tsk.LastRunAt, tsk.Overdue, tsk.MissedOccurrences = &last, false, 0
	tsk.Arming, tsk.NextRunAt = task.ArmingArmed, &next

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 14, 30, 0, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "Arming         armed")
	assert.Contains(t, got, "Next run       2026-09-01 15:20")
	assert.Contains(t, got, "Schedule       on schedule")
	assert.NotContains(t, got, "overdue")
}

// TestTasksShow_EmptyTrailSaysWhichSilenceItIs: every task written before this
// feature has an empty trail, and a blank section reads as "nothing ever
// happened to this task" — which is the exact wrong conclusion.
func TestTasksShow_EmptyTrailSaysWhichSilenceItIs(t *testing.T) {
	tsk := showFixture()
	tsk.Audit = nil

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 14, 20, 12, 0, time.Local))

	assert.Contains(t, out.String(), "no recorded changes")
}

// TestTasksShow_WatchTaskHasNoScheduleVerdict: lateness is not defined for a
// task that fires on events, so the row is absent rather than reassuring.
func TestTasksShow_WatchTaskHasNoScheduleVerdict(t *testing.T) {
	tsk := showFixture()
	tsk.CronExpr, tsk.WatchCmd = "", "tail -f ci.log"
	tsk.Overdue, tsk.MissedOccurrences = false, 0

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 14, 20, 12, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "watch · tail -f ci.log")
	assert.NotContains(t, got, "Schedule")
	assert.NotContains(t, got, "on schedule")
}

// TestTasksShowCmd_ReadsTheStoreAndScopes exercises the verb end to end on the
// disk-fallback path: no daemon, a seeded store, and the scope gate the other
// id-taking task commands share.
func TestTasksShowCmd_ReadsTheStoreAndScopes(t *testing.T) {
	useTempConfig(t)
	stubDaemon(t)
	repo := setupAddRepo(t)
	t.Cleanup(func() { repoFlag = "" })

	seedTask(t, task.Task{
		ID: "4ab7ba4f", Name: "Master Health Watch", CronExpr: "20 * * * *",
		Prompt: "sweep", ProjectPath: repo, Program: "claude", Enabled: true,
		CreatedAt: time.Now().Add(-60 * 24 * time.Hour),
	})
	// The run history goes in through its own writer — the create path discards a
	// client-supplied one (task.resetStoreOwnedFields) — so this exercises the
	// 18-day gap it means to and not a 60-day one measured from creation.
	last := time.Now().Add(-18 * 24 * time.Hour)
	_, err := task.UpdateTaskStatus("4ab7ba4f", &last, "started")
	require.NoError(t, err)

	var out bytes.Buffer
	tasksShowCmd.SetOut(&out)
	t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
	require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{"4ab7ba4f"}))

	got := out.String()
	assert.Contains(t, got, "Master Health Watch · 4ab7ba4f")
	assert.True(t, strings.Contains(got, "overdue · missed "),
		"the store read carries the derivation, with no daemon involved:\n%s", got)
	assert.Contains(t, got, "unknown — nothing has reported on it yet",
		"and it does not invent an arming observation it never made")
}

// TestTasksShow_InvalidExpressionIsNotOnSchedule: nothing can be derived from an
// expression with no occurrences, and reporting the absence of a verdict as
// "on schedule" would put a reassuring line directly above the arming line
// saying the daemon refused to schedule it.
func TestTasksShow_InvalidExpressionIsNotOnSchedule(t *testing.T) {
	tsk := showFixture()
	tsk.CronExpr = "99 * * * *"
	tsk.Overdue, tsk.MissedOccurrences = false, 0

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 14, 20, 12, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "cron expression is invalid, so nothing is scheduled")
	assert.NotContains(t, got, "on schedule")
}

// TestTasksShow_UnschedulableExpressionSaysSo: "0 0 31 2 *" parses, so the
// scheduler arms it and the arming line reads "armed" — while the task can never
// run. Reporting "on schedule" there would make every line on the page agree on
// something untrue.
func TestTasksShow_UnschedulableExpressionSaysSo(t *testing.T) {
	tsk := showFixture()
	tsk.CronExpr = "0 0 31 2 *"
	tsk.Arming = task.ArmingArmed
	tsk.Overdue, tsk.MissedOccurrences = false, 0

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 14, 20, 12, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "the scheduler cannot derive a next run from this expression")
	assert.NotContains(t, got, "on schedule")
}

// TestTasksShow_UnassessableRecordSaysSo: a record with nothing to measure from
// gets an honest "unknown", never a clean bill — it may have been firing
// perfectly and it may never have fired at all, and saying which would be
// inventing the answer.
func TestTasksShow_UnassessableRecordSaysSo(t *testing.T) {
	tsk := showFixture()
	tsk.LastRunAt, tsk.LastRunStatus = nil, ""
	tsk.CreatedAt = time.Time{}
	// The trail is cleared too: an `enabled` entry in it IS a reference point,
	// and would make this record perfectly assessable.
	tsk.Audit = nil
	tsk.Overdue, tsk.MissedOccurrences = false, 0

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "cannot be assessed — no lateness could be measured")
	assert.NotContains(t, got, "on schedule")
}

// TestTasksShow_DisabledTaskHasNoScheduleVerdict: a disabled task is excluded
// from schedule health, and the exclusion has to come BEFORE the parse — a
// disabled draft with a malformed expression was reporting "nothing is
// scheduled", which reads as a live scheduling failure, while a disabled task
// with a valid expression reported nothing at all. The scheduler skips a
// disabled task before parsing it too.
func TestTasksShow_DisabledTaskHasNoScheduleVerdict(t *testing.T) {
	for _, expr := range []string{"20 * * * *", "99 * * * *"} {
		tsk := showFixture()
		tsk.Enabled = false
		tsk.CronExpr = expr
		tsk.Overdue, tsk.MissedOccurrences = false, 0

		var out bytes.Buffer
		renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
		got := out.String()

		assert.NotContains(t, got, "Schedule", "%q: no schedule verdict for a disabled task:\n%s", expr, got)
		assert.Contains(t, got, "Enabled        no")
		assert.Contains(t, got, "cron · "+expr, "the expression is still on screen for anyone about to enable it")
	}
}

// TestTasksShow_NoCountWhenNoneWasTaken: same sentinel as the doctor row — a
// saturated zero is the shared walk budget having been spent, so the line
// reports the silence and its start without a number rather than "missed 0 or
// more" about a task proven to have missed at least one.
func TestTasksShow_NoCountWhenNoneWasTaken(t *testing.T) {
	tsk := showFixture()
	tsk.LastRunAt = nil
	tsk.CreatedAt = time.Date(2026, time.August, 14, 14, 20, 0, 0, time.Local)

	var out bytes.Buffer
	// Derived fresh here, so the fixture's own numbers are irrelevant; what
	// matters is that a zero count never renders as a bound.
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
	got := out.String()

	assert.NotContains(t, got, "missed 0", "no count is not a count of zero:\n%s", got)
}

// TestTasksShow_EnabledWithNoTriggerSaysSo: ValidateTrigger refuses to write
// one, so only a hand-edited or legacy row is enabled with neither a cron
// expression nor a watch command — and it is the emptiest kind of broken.
// Nothing schedules it, nothing watches it, and with no daemon to report arming
// the page would otherwise show "Enabled yes" and no sign of trouble.
func TestTasksShow_EnabledWithNoTriggerSaysSo(t *testing.T) {
	tsk := showFixture()
	tsk.CronExpr, tsk.WatchCmd = "", ""
	tsk.Arming = task.ArmingUnknown
	tsk.Overdue, tsk.MissedOccurrences = false, 0

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
	got := out.String()

	assert.Contains(t, got, "no trigger, so nothing will ever run it")
	assert.NotContains(t, got, "on schedule")
	assert.NotContains(t, got, "cron expression is invalid",
		"it is not a bad expression; it is the absence of one")
}

// TestTasksShow_DisabledWithNoTriggerIsSilent: a disabled draft with no trigger
// is the ordinary shape of a half-written task, and the exclusion for disabled
// tasks still comes first.
func TestTasksShow_DisabledWithNoTriggerIsSilent(t *testing.T) {
	tsk := showFixture()
	tsk.CronExpr, tsk.WatchCmd = "", ""
	tsk.Enabled = false

	var out bytes.Buffer
	renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
	assert.NotContains(t, out.String(), "Schedule")
}

// TestTasksShow_WordsEveryUnschedulableShape is show's half of the
// consolidation: one classification, three renderings, no local re-derivation.
func TestTasksShow_WordsEveryUnschedulableShape(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"", "no trigger, so nothing will ever run it"},
		{"99 * * * *", "cron expression is invalid"},
		{"0 0 31 2 *", "the scheduler cannot derive a next run"},
	} {
		tsk := showFixture()
		tsk.CronExpr, tsk.WatchCmd = tc.expr, ""
		tsk.Overdue, tsk.MissedOccurrences = false, 0

		var out bytes.Buffer
		renderTaskShow(&out, tsk, time.Date(2026, time.September, 1, 16, 30, 0, 0, time.Local))
		got := out.String()
		assert.Contains(t, got, tc.want, "%q:\n%s", tc.expr, got)
		assert.NotContains(t, got, "on schedule", "%q is not healthy", tc.expr)
	}
}
