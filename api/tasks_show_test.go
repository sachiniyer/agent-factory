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

	assert.Contains(t, out.String(), "unknown — no running daemon answered")
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

	last := time.Now().Add(-18 * 24 * time.Hour)
	seedTask(t, task.Task{
		ID: "4ab7ba4f", Name: "Master Health Watch", CronExpr: "20 * * * *",
		Prompt: "sweep", ProjectPath: repo, Program: "claude", Enabled: true,
		CreatedAt: time.Now().Add(-60 * 24 * time.Hour), LastRunAt: &last,
	})

	var out bytes.Buffer
	tasksShowCmd.SetOut(&out)
	t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
	require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{"4ab7ba4f"}))

	got := out.String()
	assert.Contains(t, got, "Master Health Watch · 4ab7ba4f")
	assert.True(t, strings.Contains(got, "overdue · missed "),
		"the store read carries the derivation, with no daemon involved:\n%s", got)
	assert.Contains(t, got, "unknown — no running daemon answered",
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
