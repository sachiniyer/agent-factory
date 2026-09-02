package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hourly is #3623's own schedule: "20 * * * *", the expression on the two tasks
// that went dark. Times are built in Local because robfig evaluates a schedule
// in time.Local unless the expression names a zone, and comparing a Local answer
// against a UTC expectation would fail on nothing but the zone.
func at(y int, mo time.Month, d, h, mi, sec int) time.Time {
	return time.Date(y, mo, d, h, mi, sec, 0, time.Local)
}

func cronTask(expr string, lastRun *time.Time) Task {
	return Task{
		ID:          "4ab7ba4f",
		Name:        "Master Health Watch",
		CronExpr:    expr,
		Prompt:      "sweep",
		ProjectPath: "/tmp/repo",
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   at(2026, time.July, 1, 9, 0, 0),
		LastRunAt:   lastRun,
	}
}

// TestOverdue_HourlyTaskDarkFor18Days is the reported bug, to the minute: an
// enabled hourly task whose last run started on 2026-08-14 14:20:08 and which
// had not fired again by 2026-09-01 14:20:12. Every surface called it healthy,
// because nothing compared those two facts. The derivation has to name the exact
// number of fires the schedule had without it — 432 — and when the silence
// started.
func TestOverdue_HourlyTaskDarkFor18Days(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	now := at(2026, time.September, 1, 14, 20, 12)
	tsk := cronTask("20 * * * *", &last)
	tsk.LastRunStatus = "started"

	health := DeriveScheduleHealth(tsk, now)

	require.True(t, health.Overdue,
		"an hourly task silent for 18 days is the bug this derivation exists to catch")
	assert.Equal(t, 432, health.MissedOccurrences,
		"18 days of an hourly schedule is 432 fires the task did not take")
	assert.False(t, health.Saturated, "432 is well under the cap and must be exact")
	assert.True(t, at(2026, time.August, 14, 15, 20, 0).Equal(health.OldestMissedAt),
		"the silence starts at the first occurrence after the last run, not at the end of the slack window")
	assert.Equal(t, time.Hour, health.Slack, "an hourly schedule's slack is one hour")
}

// TestOverdue_SlackBoundary pins both sides of the window. The rule is "more
// than one full period late", so a task that is late by EXACTLY its slack is
// still on schedule — that lateness is indistinguishable from a fire that is
// simply in progress.
func TestOverdue_SlackBoundary(t *testing.T) {
	last := at(2026, time.September, 1, 14, 20, 0)
	tsk := cronTask("20 * * * *", &last)

	t.Run("late by exactly one period is not overdue", func(t *testing.T) {
		// now is 15:20:00: the most recent occurrence is 15:20, one hour after the
		// last run, and the slack is one hour.
		assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 15, 20, 0)).Overdue)
	})
	t.Run("one second short of two periods is not overdue", func(t *testing.T) {
		assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 16, 19, 59)).Overdue)
	})
	t.Run("two full periods is overdue", func(t *testing.T) {
		health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 16, 20, 0))
		require.True(t, health.Overdue)
		assert.Equal(t, 2, health.MissedOccurrences, "15:20 and 16:20 both went unfired")
	})
}

// TestOverdue_SlackFloorIsFiveMinutes: the window is max(one period, 5 minutes),
// so a per-minute schedule is not called late on a single skipped minute. Both
// assertions here would fail if the floor were dropped — a 1-minute slack would
// flag the first case.
func TestOverdue_SlackFloorIsFiveMinutes(t *testing.T) {
	last := at(2026, time.September, 1, 12, 0, 0)
	tsk := cronTask("* * * * *", &last)

	require.Equal(t, MinSlack, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 3, 0)).Slack)
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 3, 0)).Overdue,
		"three missed minutes is inside the five-minute floor")
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 5, 0)).Overdue,
		"exactly five minutes is the boundary, and the boundary is not overdue")

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 6, 0))
	require.True(t, health.Overdue, "past the floor the task is late")
	assert.Equal(t, 6, health.MissedOccurrences)
}

// TestOverdue_SlackScalesWithTheSchedule: a daily task is owed a day, not five
// minutes. Sharing one fixed window across every schedule would either scream at
// every nightly task or never catch a per-minute one.
func TestOverdue_SlackScalesWithTheSchedule(t *testing.T) {
	last := at(2026, time.September, 1, 3, 0, 0)
	tsk := cronTask("0 3 * * *", &last)

	require.Equal(t, 24*time.Hour, DeriveScheduleHealth(tsk, at(2026, time.September, 2, 4, 0, 0)).Slack)
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 2, 4, 0, 0)).Overdue,
		"one skipped daily run is inside the day of slack")

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 3, 3, 30, 0))
	require.True(t, health.Overdue)
	assert.Equal(t, 2, health.MissedOccurrences)
}

// TestOverdue_SlackSpansTheGapItMustBridge: the slack is the WIDER of the two
// gaps around the last run, so a weekdays-only task whose last run was a Friday
// is owed the whole weekend. Judged by the Monday-to-Tuesday period alone (24h)
// it would be called overdue the instant Monday 09:00 arrived — flagging a fire
// that is due right now as one that was missed.
func TestOverdue_SlackSpansTheGapItMustBridge(t *testing.T) {
	// 2026-09-04 is a Friday.
	friday := at(2026, time.September, 4, 9, 0, 0)
	tsk := cronTask("0 9 * * 1-5", &friday)
	require.Equal(t, 72*time.Hour, DeriveScheduleHealth(tsk, at(2026, time.September, 6, 12, 0, 0)).Slack)
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 7, 9, 0, 0)).Overdue,
		"Monday's fire is due, not missed")
	assert.True(t, DeriveScheduleHealth(tsk, at(2026, time.September, 8, 9, 0, 0)).Overdue,
		"once Monday AND Tuesday have passed unfired the task is late")
}

// TestOverdue_NeverRunMeasuresFromCreation: the most broken task of all — one
// created and then never armed — has no LastRunAt, and treating that as "nothing
// to compare against" would make it the one task that always reads healthy.
func TestOverdue_NeverRunMeasuresFromCreation(t *testing.T) {
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = at(2026, time.September, 1, 9, 0, 0)

	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 9, 30, 0)).Overdue,
		"a task created half an hour ago is inside its first window, not behind")
	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 11, 0, 0))
	require.True(t, health.Overdue,
		"a task that has never run and is two occurrences past its creation is exactly the case that used to read healthiest")
	assert.Equal(t, 2, health.MissedOccurrences, "09:20 and 10:20 both went unfired")
	assert.True(t, at(2026, time.September, 1, 9, 20, 0).Equal(health.OldestMissedAt))
}

// TestOverdue_NotDerivedWhereItWouldBeMeaningless covers every shape the
// derivation must decline, each for its own reason.
func TestOverdue_NotDerivedWhereItWouldBeMeaningless(t *testing.T) {
	long := at(2020, time.January, 1, 0, 0, 0)
	now := at(2026, time.September, 1, 12, 0, 0)

	t.Run("watch task", func(t *testing.T) {
		tsk := Task{ID: "w", WatchCmd: "tail -f x", Enabled: true, LastRunAt: &long}
		assert.False(t, DeriveScheduleHealth(tsk, now).Overdue,
			"a watch command that emits nothing for years may be working perfectly")
	})
	t.Run("disabled task", func(t *testing.T) {
		tsk := cronTask("20 * * * *", &long)
		tsk.Enabled = false
		assert.False(t, DeriveScheduleHealth(tsk, now).Overdue,
			"a disabled task is not expected to fire; whether disabling it was intended is the audit trail's question")
	})
	t.Run("unparseable expression", func(t *testing.T) {
		tsk := cronTask("not a cron", &long)
		health := DeriveScheduleHealth(tsk, now)
		assert.False(t, health.Overdue, "an expression with no occurrences was never due")
		assert.True(t, health.Unschedulable, "but it can never fire, which is its own verdict")
	})
	t.Run("expression that matches no date", func(t *testing.T) {
		// February 31st: legal syntax, never occurs. Next() gives up after five
		// years and returns the zero time.
		tsk := cronTask("0 0 31 2 *", &long)
		health := DeriveScheduleHealth(tsk, now)
		assert.False(t, health.Overdue)
		assert.True(t, health.Unschedulable)
	})
	t.Run("record with no timestamps at all", func(t *testing.T) {
		tsk := cronTask("20 * * * *", nil)
		tsk.CreatedAt = time.Time{}
		assert.False(t, DeriveScheduleHealth(tsk, now).Overdue,
			"measuring from the zero time would report every hand-edited row as millions of fires behind")
	})
}

// TestOverdue_MissedCountSaturates: the count is produced by walking the
// schedule, so it must be bounded — but the VERDICT must survive the bound. A
// per-minute task dark for a year is still overdue, and the number is reported
// as a floor rather than quietly truncated into a lie.
func TestOverdue_MissedCountSaturates(t *testing.T) {
	last := at(2025, time.September, 1, 12, 0, 0)
	tsk := cronTask("* * * * *", &last)

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 0, 0))
	require.True(t, health.Overdue)
	assert.Equal(t, MaxMissedOccurrences, health.MissedOccurrences)
	assert.True(t, health.Saturated, "a saturated count must announce that it is a floor")
}

// TestWithScheduleHealth_AnnotatesEveryRecord proves the slice-level entry point
// carries the verdict onto the fields the surfaces actually read.
func TestWithScheduleHealth_AnnotatesEveryRecord(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	now := at(2026, time.September, 1, 14, 20, 12)
	healthy := cronTask("20 * * * *", &now)
	healthy.ID = "healthy1"
	dark := cronTask("20 * * * *", &last)

	got := WithScheduleHealth([]Task{healthy, dark}, now)

	assert.False(t, got[0].Overdue)
	assert.Zero(t, got[0].MissedOccurrences)
	assert.True(t, got[1].Overdue)
	assert.Equal(t, 432, got[1].MissedOccurrences)
}

// TestScheduleHealthIsNeverPersisted is the invariant that keeps "derived" true.
// A stored overdue flag is a claim about an instant that has already passed by
// the time anything reads it back, so the write path erases the fields even when
// the record it is handed carries them.
func TestScheduleHealthIsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	last := at(2026, time.August, 14, 14, 20, 8)
	tsk := cronTask("20 * * * *", nil)
	tsk.ID = "persist1"
	// A caller handing the store a fully-annotated record — exactly what a
	// load-modify-save path would do now that every load annotates.
	next := at(2026, time.September, 1, 15, 20, 0)
	tsk.Overdue, tsk.MissedOccurrences, tsk.Arming, tsk.NextRunAt = true, 432, ArmingArmed, &next
	tsk.Unschedulable, tsk.UnschedulableReason = true, ReasonInvalidExpression
	require.NoError(t, AddTask(tsk))
	// The run history comes from its own writer, not from the create: a create
	// supplies the task's definition and the store supplies its history (see
	// resetStoreOwnedFields).
	require.NoError(t, UpdateTaskStatus("persist1", &last, "started"))

	raw, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	require.NoError(t, err)
	for _, key := range []string{"overdue", "missed_occurrences", "next_run_at", "arming", "unschedulable", "unschedulable_reason"} {
		assert.NotContains(t, string(raw), key,
			"a derived field must never reach disk: %s", key)
	}

	// And the same read that would have shown a stale flag re-derives it instead.
	restore := nowFn
	nowFn = func() time.Time { return at(2026, time.September, 1, 14, 20, 12) }
	defer func() { nowFn = restore }()
	loaded, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.True(t, loaded[0].Overdue, "every read derives the verdict fresh")
	assert.Equal(t, 432, loaded[0].MissedOccurrences)
	assert.Equal(t, ArmingUnknown, loaded[0].Arming,
		"a disk read has not observed arming and must not claim to have")
	assert.Nil(t, loaded[0].NextRunAt,
		"a next-run time can only come from a live scheduler entry")
	assert.False(t, loaded[0].Unschedulable, "and the forged verdict is re-derived, not restored")
	assert.Empty(t, loaded[0].UnschedulableReason)
}

// TestLoadTasksDerivationSurvivesTheJSONShape guards the wire form the CLI and
// web read: the fields must be present when true and absent when false, so
// `af tasks list --json` neither loses the signal nor grows noise on healthy
// rows.
func TestLoadTasksDerivationSurvivesTheJSONShape(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	dark := cronTask("20 * * * *", &last)
	encoded, err := json.Marshal(WithScheduleHealth([]Task{dark}, at(2026, time.September, 1, 14, 20, 12))[0])
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"overdue":true`)
	assert.Contains(t, string(encoded), `"missed_occurrences":432`)

	healthy := cronTask("20 * * * *", &last)
	healthy.Enabled = false
	encoded, err = json.Marshal(WithScheduleHealth([]Task{healthy}, at(2026, time.September, 1, 14, 20, 12))[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "overdue")
	assert.NotContains(t, string(encoded), "missed_occurrences")
}

// TestOverdue_ReEnableRestartsTheClock is #3623's own episode, from the
// operator's side: the tasks were disabled on 2026-08-14 when the fleet was
// paused and re-enabled on 2026-09-01. Measuring from the last RUN would report
// all 432 occurrences they missed while intentionally off, from the moment they
// came back until their next fire — a true statement about the past and a
// useless one about the present. A re-enable is a fresh start, exactly as a
// first run is.
func TestOverdue_ReEnableRestartsTheClock(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	tsk := cronTask("20 * * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.August, 14, 14, 30, 0), Actor: ActorCLI, Action: AuditDisabled, Fields: []string{"enabled"}},
		{At: at(2026, time.September, 1, 14, 5, 0), Actor: ActorCLI, Action: AuditEnabled, Fields: []string{"enabled"}},
	}

	// Back on at 14:05, its first occurrence since is 14:20. Nothing is late yet.
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 14, 10, 0)).Overdue,
		"the pause it was deliberately in is not a backlog it now owes")
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 15, 0, 0)).Overdue,
		"nor is the fire it has not reached yet")

	// Once the first post-enable occurrence has gone unfired past the window, it
	// is late again — and about the right number of fires.
	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 15, 20, 0))
	require.True(t, health.Overdue, "a task that stops firing AFTER being re-enabled is still overdue")
	assert.Equal(t, 2, health.MissedOccurrences,
		"counted from the re-enable, not from a run 18 days before it")
	assert.True(t, at(2026, time.September, 1, 14, 20, 0).Equal(health.OldestMissedAt),
		"the silence starts at the first occurrence after it came back")
}

// TestOverdue_ReEnableOnADailySchedule is the shape that made this worth fixing:
// a nightly task switched back on mid-morning would otherwise claim a month of
// misses for most of a day, every time anyone paused it.
func TestOverdue_ReEnableOnADailySchedule(t *testing.T) {
	last := at(2026, time.August, 2, 4, 53, 0)
	tsk := cronTask("53 4 * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 10, 0, 0), Actor: ActorTUI, Action: AuditEnabled, Fields: []string{"enabled"}},
	}

	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 10, 30, 0)).Overdue,
		"re-enabled half an hour ago, and its next fire is tonight")
	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 2, 5, 0, 0)).Overdue,
		"its first post-enable fire is due, not missed")
	assert.True(t, DeriveScheduleHealth(tsk, at(2026, time.September, 3, 5, 0, 0)).Overdue,
		"two nights unfired since it came back is a real silence")
}

// TestOverdue_OlderEnableDoesNotOutrankANewerRun: the reference is the LATEST of
// the two, so a task that has actually run since being re-enabled measures from
// the run.
func TestOverdue_OlderEnableDoesNotOutrankANewerRun(t *testing.T) {
	last := at(2026, time.September, 1, 14, 20, 0)
	tsk := cronTask("20 * * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.August, 20, 9, 0, 0), Actor: ActorCLI, Action: AuditEnabled, Fields: []string{"enabled"}},
	}

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 16, 20, 0))
	require.True(t, health.Overdue)
	assert.Equal(t, 2, health.MissedOccurrences, "measured from the run, not the older enable")
}

// TestOverdue_EnableWithNoRunAtAllIsStillMeasurable: a task enabled but never
// fired has nothing but the enable to measure from, and that is exactly the task
// most worth catching.
func TestOverdue_EnableWithNoRunAtAllIsStillMeasurable(t *testing.T) {
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = at(2026, time.June, 1, 9, 0, 0)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 9, 0, 0), Actor: ActorCLI, Action: AuditEnabled, Fields: []string{"enabled"}},
	}

	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 9, 30, 0)).Overdue,
		"three months of pre-enable silence is not this task's backlog")
	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 11, 0, 0))
	require.True(t, health.Overdue, "but two unfired occurrences since it came on are")
	assert.Equal(t, 2, health.MissedOccurrences)
}

// TestOverdue_MalformedEnableEntryDoesNotShadowARealOne: a hand-edited or
// truncated row with no timestamp must not be answered with — measuring from the
// zero time would report the task as millions of occurrences overdue, and
// stopping at it would hide the real enable behind it.
func TestOverdue_MalformedEnableEntryDoesNotShadowARealOne(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	tsk := cronTask("20 * * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 14, 5, 0), Actor: ActorCLI, Action: AuditEnabled},
		{Actor: ActorCLI, Action: AuditEnabled}, // no timestamp at all
	}

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 15, 20, 0))
	require.True(t, health.Overdue)
	assert.Equal(t, 2, health.MissedOccurrences,
		"the real enable behind the malformed entry is what the clock restarts from")
}

// TestOverdue_UnschedulableIsReportedNotIgnored: "0 0 31 2 *" is February 31st —
// legal syntax that matches no date. Parsing succeeds, so the scheduler arms it
// and holds an entry with a zero next-fire time; nothing is ever late because
// nothing is ever due. Reporting the absence of a verdict as health would be the
// worst answer available, since the task is enabled, armed, and incapable of
// firing.
func TestOverdue_UnschedulableIsReportedNotIgnored(t *testing.T) {
	last := at(2026, time.January, 1, 0, 0, 0)
	tsk := cronTask("0 0 31 2 *", &last)

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 0, 0))
	assert.False(t, health.Overdue, "nothing was ever due, so nothing is late")
	assert.True(t, health.Unschedulable,
		"but the task can never fire, and that is a report rather than a silence")
}

// TestOverdue_SchedulableTasksAreNotFlaggedUnschedulable guards the other side:
// the flag means "no occurrences at all", not "cannot derive".
func TestOverdue_SchedulableTasksAreNotFlaggedUnschedulable(t *testing.T) {
	last := at(2026, time.September, 1, 14, 20, 0)
	assert.False(t, DeriveScheduleHealth(cronTask("20 * * * *", &last), at(2026, time.September, 1, 15, 0, 0)).Unschedulable)
	assert.True(t, DeriveScheduleHealth(cronTask("not a cron", &last), at(2026, time.September, 1, 15, 0, 0)).Unschedulable,
		"an expression that does not parse can never fire either, and the verdict must come from the "+
			"record: leaning on a live not-armed observation left a box with no daemon calling it healthy")

	watch := Task{ID: "w", WatchCmd: "tail -f x", Enabled: true, LastRunAt: &last}
	assert.False(t, DeriveScheduleHealth(watch, at(2026, time.September, 1, 15, 0, 0)).Unschedulable)
}

// TestOverdue_RetimingRestartsTheClock is the enable rule's twin, and just as
// sharp: retiming a daily task to every minute at 23:00 would otherwise be
// measured against this morning's run and report a day's worth of per-minute
// misses that happened before that schedule existed.
func TestOverdue_RetimingRestartsTheClock(t *testing.T) {
	last := at(2026, time.September, 1, 3, 0, 0)
	tsk := cronTask("* * * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 23, 0, 0), Actor: ActorCLI, Action: AuditUpdated, Fields: []string{"cron_expr"}},
	}

	assert.False(t, DeriveScheduleHealth(tsk, at(2026, time.September, 1, 23, 3, 0)).Overdue,
		"three minutes on the new schedule is inside its window, whatever the old one did")
	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 23, 10, 0))
	require.True(t, health.Overdue, "ten minutes of a per-minute schedule going unfired is real")
	assert.Equal(t, 10, health.MissedOccurrences,
		"counted from the retime, not from a run twenty hours before it")
}

// TestOverdue_AnOrdinaryEditRestartsNothing: only a change to the SCHEDULE moves
// the reference. Editing the prompt does not excuse a task from the fires it has
// been missing.
func TestOverdue_AnOrdinaryEditRestartsNothing(t *testing.T) {
	last := at(2026, time.August, 14, 14, 20, 8)
	tsk := cronTask("20 * * * *", &last)
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 14, 0, 0), Actor: ActorTUI, Action: AuditUpdated, Fields: []string{"prompt", "program"}},
	}

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 14, 20, 12))
	require.True(t, health.Overdue)
	assert.Equal(t, 432, health.MissedOccurrences,
		"the schedule did not move, so neither did the reference")
}

// TestWithScheduleHealth_CarriesSaturation: the record must not render a capped
// count as an exact one — "10000" and "at least 10000" call for the same action,
// but only one of them is true.
func TestWithScheduleHealth_CarriesSaturation(t *testing.T) {
	last := at(2025, time.September, 1, 12, 0, 0)
	got := WithScheduleHealth([]Task{cronTask("* * * * *", &last)}, at(2026, time.September, 1, 12, 0, 0))

	require.True(t, got[0].Overdue)
	assert.Equal(t, MaxMissedOccurrences, got[0].MissedOccurrences)
	assert.True(t, got[0].MissedOccurrencesCapped,
		"a reader of the record alone must be able to tell a floor from an exact count")

	encoded, err := json.Marshal(got[0])
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"missed_occurrences_capped":true`)
}

// TestOverdue_UnschedulableRidesTheRecord: only `af tasks show` and `af doctor`
// recompute health; every other consumer — `af tasks list --json`, the web, the
// TUI rail — reads the record. A verdict that lives only in ScheduleHealth is a
// verdict those surfaces cannot see, so they would render a task that can never
// fire exactly like a healthy one.
func TestOverdue_UnschedulableRidesTheRecord(t *testing.T) {
	last := at(2026, time.January, 1, 0, 0, 0)
	for _, expr := range []string{"0 0 31 2 *", "not a cron"} {
		got := WithScheduleHealth([]Task{cronTask(expr, &last)}, at(2026, time.September, 1, 12, 0, 0))
		assert.True(t, got[0].Unschedulable, "expression %q", expr)
		assert.False(t, got[0].Overdue, "expression %q was never due", expr)

		encoded, err := json.Marshal(got[0])
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"unschedulable":true`)
	}

	healthy := WithScheduleHealth([]Task{cronTask("20 * * * *", &last)}, at(2026, time.September, 1, 12, 0, 0))
	encoded, err := json.Marshal(healthy[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "unschedulable", "and it is absent when it does not apply")
}

// TestOverdue_UnschedulableWithNoTimestampsAtAll: "can this ever fire?" needs no
// reference point, so it must be answered before "how late is it?", which does.
// Asked the other way round, a hand-edited row with neither a run nor a creation
// time declined to derive anything and an impossible expression reported healthy
// — the one case where every input the verdict needs was already in hand.
func TestOverdue_UnschedulableWithNoTimestampsAtAll(t *testing.T) {
	now := at(2026, time.September, 1, 12, 0, 0)
	for _, expr := range []string{"0 0 31 2 *", "not a cron"} {
		tsk := cronTask(expr, nil)
		tsk.CreatedAt = time.Time{}
		require.False(t, func() bool { _, ok := scheduleReference(tsk); return ok }(),
			"precondition: %q has no reference point at all", expr)

		health := DeriveScheduleHealth(tsk, now)
		assert.True(t, health.Unschedulable, "expression %q can never fire, timestamps or not", expr)
		assert.False(t, health.Overdue, "and it was never due")
	}

	// A schedulable expression with no timestamps still derives nothing: there is
	// genuinely nothing to measure from, and inventing one would report every such
	// row as millions of occurrences behind.
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = time.Time{}
	assert.False(t, DeriveScheduleHealth(tsk, now).Unschedulable)
	assert.False(t, DeriveScheduleHealth(tsk, now).Overdue)
}

// TestOverdue_UnschedulableIsAClaimAboutTheScheduler pins the case that decides
// the wording, and guards against someone "correcting" it into a false negative.
//
// "0 0 29 2 *" is a perfectly valid leap-day expression, but asked in 2096 its
// next match is 2104 — 2100 is not a leap year — which is past the five-year
// horizon robfig's Next() searches before returning the zero time. The SCHEDULER
// consults that same horizon, so during that window it holds a zero next-fire
// time and does not run the task. Reporting health there would be the false
// negative; reporting "can never fire" would be a false claim about the
// calendar. Unschedulable means the scheduler cannot derive a next run, which is
// true in both cases and is the thing to act on.
func TestOverdue_UnschedulableIsAClaimAboutTheScheduler(t *testing.T) {
	last := at(2096, time.February, 29, 0, 0, 0)
	leapDay := cronTask("0 0 29 2 *", &last)

	inTheGap := DeriveScheduleHealth(leapDay, at(2096, time.June, 1, 0, 0, 0))
	assert.True(t, inTheGap.Unschedulable,
		"the scheduler cannot derive a next run here, so neither can this")
	assert.False(t, inTheGap.Overdue, "and nothing was due to be missed")

	// Past the gap the same expression schedules normally — and note what that
	// costs: from 2099 the schedule can be fired (2104 is inside the horizon from
	// now) while nothing can be MEASURED from a 2096 last run, whose own next
	// occurrences fall outside it. That is "no lateness verdict", not
	// "unschedulable", and conflating the two would claim the scheduler cannot
	// fire a task it is about to.
	pastTheGap := DeriveScheduleHealth(leapDay, at(2099, time.June, 1, 0, 0, 0))
	assert.False(t, pastTheGap.Unschedulable,
		"2104 is inside the horizon from 2099, so the scheduler can fire it again")
	assert.False(t, pastTheGap.Overdue, "and no lateness can be measured from 2096")
	recent := at(2024, time.February, 29, 0, 0, 0)
	ordinary := cronTask("0 0 29 2 *", &recent)
	assert.False(t, DeriveScheduleHealth(ordinary, at(2026, time.June, 1, 0, 0, 0)).Unschedulable)
}

// TestOverdue_NoReferenceIsUnknownNotHealthy: the store stamps CreatedAt on
// every create now, so only a hand-edited or truncated row can reach this — but
// reporting such a row as "on schedule" would be the exact lie this feature
// exists to remove, told about a task that may never have fired.
func TestOverdue_NoReferenceIsUnknownNotHealthy(t *testing.T) {
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = time.Time{}

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 12, 0, 0))
	assert.True(t, health.Unassessable, "nothing to measure from is a verdict of its own")
	assert.False(t, health.Overdue, "and it is not a claim that the task is late")
	assert.False(t, health.Unschedulable, "nor that the expression is bad — it is fine")

	// It reaches the record, so the surfaces that read one rather than
	// recomputing get it too.
	got := WithScheduleHealth([]Task{tsk}, at(2026, time.September, 1, 12, 0, 0))
	assert.True(t, got[0].Unassessable)
	encoded, err := json.Marshal(got[0])
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"unassessable":true`)

	// A record with a reference is assessed normally and carries no such flag.
	fine := cronTask("20 * * * *", nil)
	fine.CreatedAt = at(2026, time.September, 1, 11, 0, 0)
	assert.False(t, DeriveScheduleHealth(fine, at(2026, time.September, 1, 11, 30, 0)).Unassessable)
}

// TestOverdue_AuditTrailRescuesARecordWithNoTimestamps documents the fallback
// order, and the reason it matters: a task re-enabled through any surface gains
// a reference point from the trail even if its own timestamps were lost, so the
// unassessable verdict is genuinely last resort rather than the common answer
// for legacy rows.
func TestOverdue_AuditTrailRescuesARecordWithNoTimestamps(t *testing.T) {
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = time.Time{}
	tsk.Audit = []AuditEntry{
		{At: at(2026, time.September, 1, 9, 0, 0), Actor: ActorCLI, Action: AuditEnabled, Fields: []string{"enabled"}},
	}

	health := DeriveScheduleHealth(tsk, at(2026, time.September, 1, 11, 0, 0))
	assert.False(t, health.Unassessable, "the enable is a reference point")
	assert.True(t, health.Overdue, "and two unfired occurrences since it is a real silence")
	assert.Equal(t, 2, health.MissedOccurrences)
}

// TestOverdue_EvaluatesInTheSchedulerLocation: robfig treats a schedule with no
// explicit timezone as local to the time it is HANDED (spec.go: "schedules
// without a time zone specified (time.Local) are treated as local to the time
// provided"), so the location riding the reference decides where the cron lands.
//
// That location is not reliably the daemon's: an HTTP client can send created_at
// as UTC and the store preserves it, and a record read back over gob or JSON
// carries a fixed-offset zone rather than a named one. Evaluated there, a
// midnight task is judged against midnight somewhere else — and disagrees with
// the scheduler that actually fires it.
func TestOverdue_EvaluatesInTheSchedulerLocation(t *testing.T) {
	// A zone far from UTC, pinned rather than borrowed from the host.
	zone := time.FixedZone("PDT", -7*60*60)
	created := time.Date(2026, time.August, 31, 0, 30, 0, 0, zone)

	local := cronTask("0 0 * * *", nil) // midnight daily
	local.CreatedAt = created
	utc := local
	utc.CreatedAt = created.UTC() // same instant, UTC location — what a client sends

	// 17:01 the next day in that zone: the second local midnight has not arrived,
	// so nothing is late. Evaluated in UTC the same instant is past two UTC
	// midnights, which is how the verdicts came apart.
	now := time.Date(2026, time.September, 1, 17, 1, 0, 0, zone)
	assert.False(t, DeriveScheduleHealth(local, now).Overdue)
	assert.False(t, DeriveScheduleHealth(utc, now).Overdue,
		"the same instant stored in a different location must reach the same verdict")

	// And both agree once it really is late.
	late := time.Date(2026, time.September, 2, 0, 1, 0, 0, zone)
	assert.True(t, DeriveScheduleHealth(local, late).Overdue)
	assert.True(t, DeriveScheduleHealth(utc, late).Overdue)
	assert.Equal(t,
		DeriveScheduleHealth(local, late).MissedOccurrences,
		DeriveScheduleHealth(utc, late).MissedOccurrences)
}

// TestWithScheduleHealth_BoundsWorkAcrossTheWholeLoad is the one that matters
// for the machine this runs on. A per-task cap bounds nothing in aggregate: the
// store holds an unbounded number of tasks and the Automations rail derives all
// of them every 750ms, so a hundred long-dark per-minute tasks cost a hundred
// times the cap — refreshes overlapping continuously while nothing changes.
//
// The budget is shared, so it costs precision and never the verdict: every task
// is still correctly overdue, and the counts say they are floors.
func TestWithScheduleHealth_BoundsWorkAcrossTheWholeLoad(t *testing.T) {
	now := at(2026, time.September, 1, 12, 0, 0)
	last := now.AddDate(-1, 0, 0)
	var tasks []Task
	for i := 0; i < 100; i++ {
		tsk := cronTask("* * * * *", &last)
		tsk.ID = fmt.Sprintf("dark%03d", i)
		tasks = append(tasks, tsk)
	}

	// The WORK is asserted directly, not timed. This was a wall-clock ceiling
	// until #3674, and that oracle failed in both directions: it measured the
	// runner rather than the code, and the gap it had to resolve — roughly 0.5s
	// shared against 2.4s unbounded on a quiet machine — is inside the noise band
	// of a loaded macOS runner, so it reddened PRs whose diffs touched nothing
	// here while the property it guarded still held.
	//
	// Two assertions, and the pair is the point. The BOUND says the load spent one
	// budget rather than one per task. The EQUALITY ties that reported spend to
	// the counts on the records, so the bound cannot be satisfied by a batch that
	// reset per task and reported only its last budget: it would have to
	// under-report the work by exactly the amount the records show it did.
	healths, spent := deriveScheduleHealthBatch(tasks, now)

	assert.LessOrEqual(t, spent, MaxMissedOccurrences,
		"a load of long-dark high-frequency tasks must not cost more than ONE derivation's budget; "+
			"a per-task budget would spend up to %d", len(tasks)*MaxMissedOccurrences)

	counted := 0
	for i, h := range healths {
		assert.True(t, h.Overdue, "dark%03d: the verdict survives the budget", i)
		counted += h.MissedOccurrences
	}
	assert.Equal(t, spent, counted,
		"every step the load reports spending must be one it can show on a record, "+
			"or the bound above is measuring something other than the work")

	// And the same load through the public entry point still annotates the
	// records, which is what every read path actually calls.
	got := WithScheduleHealth(tasks, now)
	total := 0
	for _, tsk := range got {
		assert.True(t, tsk.Overdue, "%s: the verdict survives the budget", tsk.ID)
		total += tsk.MissedOccurrences
	}
	assert.LessOrEqual(t, total, MaxMissedOccurrences,
		"the whole load spends one budget between them")
	assert.True(t, got[0].MissedOccurrencesCapped,
		"and a count cut short by the budget says it is a floor")
	assert.True(t, got[99].Overdue, "including the tasks the budget never reached")
	assert.True(t, got[99].MissedOccurrencesCapped,
		"which report no count rather than the one occurrence a single step would buy")
	assert.Zero(t, got[99].MissedOccurrences)
}

// TestDeriveScheduleHealth_KeepsItsOwnBudget: a single derivation is unaffected
// by the sharing — it is the whole budget on its own, so the documented
// saturation point still means what it says.
func TestDeriveScheduleHealth_KeepsItsOwnBudget(t *testing.T) {
	last := at(2025, time.September, 1, 12, 0, 0)
	health := DeriveScheduleHealth(cronTask("* * * * *", &last), at(2026, time.September, 1, 12, 0, 0))
	assert.Equal(t, MaxMissedOccurrences, health.MissedOccurrences)
	assert.True(t, health.Saturated)
}

// TestDeriveScheduleHealthBatch_SharesOneBudget: the batch exists so that a
// caller deriving many tasks cannot accidentally reset the cap per record — the
// doctor pass did exactly that after the load path had been fixed, which is how
// a per-task cap keeps quietly becoming a task-count × cap cost.
func TestDeriveScheduleHealthBatch_SharesOneBudget(t *testing.T) {
	now := at(2026, time.September, 1, 12, 0, 0)
	last := now.AddDate(-1, 0, 0)
	var tasks []Task
	for i := 0; i < 50; i++ {
		tsk := cronTask("* * * * *", &last)
		tsk.ID = fmt.Sprintf("dark%03d", i)
		tasks = append(tasks, tsk)
	}

	started := time.Now()
	healths := DeriveScheduleHealthBatch(tasks, now)
	elapsed := time.Since(started)

	require.Len(t, healths, len(tasks), "positionally aligned with its input")
	assert.Less(t, elapsed, 500*time.Millisecond)
	total := 0
	for i, h := range healths {
		assert.True(t, h.Overdue, "task %d keeps its verdict", i)
		total += h.MissedOccurrences
	}
	assert.LessOrEqual(t, total, MaxMissedOccurrences, "one budget between all of them")
}

// TestWithScheduleHealth_ClearsDiskSourcedLiveFields: af never writes the live
// fields — saveTasks strips them — but a hand-edited or externally generated
// tasks.json can carry them, and they decode like any other field. Without
// clearing, a disk read with no daemon anywhere would report a task as armed
// with a next run someone typed: an observation nobody made, which is the one
// thing the arming tri-state exists to prevent.
func TestWithScheduleHealth_ClearsDiskSourcedLiveFields(t *testing.T) {
	typed := at(2030, time.January, 1, 0, 0, 0)
	tsk := cronTask("20 * * * *", nil)
	tsk.CreatedAt = at(2026, time.September, 1, 11, 0, 0)
	tsk.Arming, tsk.NextRunAt = ArmingArmed, &typed

	got := WithScheduleHealth([]Task{tsk}, at(2026, time.September, 1, 11, 30, 0))

	assert.Equal(t, ArmingUnknown, got[0].Arming,
		"a read has observed nothing live, whatever the file said")
	assert.Nil(t, got[0].NextRunAt, "and promises no fire time it did not read off a scheduler")
}

// TestUnschedulableReason_IsTheOneClassification: three surfaces word this
// condition and each was re-deriving it from ParseCron, so each got a different
// subset wrong — an absent expression reported as an invalid one, twice, and a
// doctor row telling the operator to correct an expression that did not exist.
// Renderers may differ; the classification must not.
func TestUnschedulableReason_IsTheOneClassification(t *testing.T) {
	now := at(2026, time.September, 1, 12, 0, 0)
	last := at(2026, time.September, 1, 11, 0, 0)

	for _, tc := range []struct {
		name string
		mut  func(*Task)
		want string
	}{
		{"no trigger", func(t *Task) { t.CronExpr = "" }, ReasonNoTrigger},
		{"whitespace trigger", func(t *Task) { t.CronExpr = "   " }, ReasonNoTrigger},
		{"invalid expression", func(t *Task) { t.CronExpr = "99 * * * *" }, ReasonInvalidExpression},
		{"no occurrence", func(t *Task) { t.CronExpr = "0 0 31 2 *" }, ReasonNoOccurrence},
		{"schedulable", func(t *Task) { t.CronExpr = "20 * * * *" }, ""},
		{"disabled", func(t *Task) { t.CronExpr, t.Enabled = "", false }, ""},
		{"watch", func(t *Task) { t.CronExpr, t.WatchCmd = "", "tail -f x" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tsk := cronTask("20 * * * *", &last)
			tc.mut(&tsk)
			assert.Equal(t, tc.want, UnschedulableReason(tsk, now))
			// And the verdict agrees with its own classifier, in both directions.
			assert.Equal(t, tc.want != "", DeriveScheduleHealth(tsk, now).Unschedulable,
				"the derivation and the classification cannot disagree")
		})
	}
}

// TestLoadTasksLocked_CarriesNoDerivedState: records loaded on the WRITE path do
// not only go back to disk — they ride EventTaskUpdated to every connected
// client, from the legacy RepoID backfill and from an update whose post-commit
// reload failed. A hand-edited tasks.json carrying live fields would otherwise
// have them decoded and broadcast as though a daemon had observed them.
func TestLoadTasksLocked_CarriesNoDerivedState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	path := filepath.Join(dir, tasksFileName)

	// Hand-edited: current schema, with live and derived fields set by hand.
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1,"tasks":[{
      "id":"handedit","name":"Hand edited","prompt":"p","cron_expr":"20 * * * *",
      "project_path":"/tmp","program":"claude","enabled":true,
      "created_at":"2026-07-01T09:00:00Z",
      "arming":"armed","next_run_at":"2030-01-01T00:00:00Z",
      "overdue":true,"missed_occurrences":99,"unschedulable":true,"unassessable":true}]}`), 0o644))

	loaded, err := loadTasksLocked(path)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Empty(t, loaded[0].Arming, "no observation the write path never made")
	assert.Nil(t, loaded[0].NextRunAt)
	assert.False(t, loaded[0].Overdue)
	assert.Zero(t, loaded[0].MissedOccurrences)
	assert.False(t, loaded[0].Unschedulable)
	assert.False(t, loaded[0].Unassessable)
	assert.Equal(t, "20 * * * *", loaded[0].CronExpr, "and the task itself is untouched")
}

// TestScheduleHealthCarriesTheUnschedulableReason: the verdict is one thing, but
// its WORDING is three, and a surface that cannot call UnschedulableReason has to
// read the classifier's answer rather than invent a fourth classification from
// cron_expr — which is what had other surfaces calling an ABSENT expression
// invalid (#3648). The field is the wire form of the classifier, so it must agree
// with it on every shape.
func TestScheduleHealthCarriesTheUnschedulableReason(t *testing.T) {
	now := at(2026, time.September, 1, 14, 20, 0)
	for name, tc := range map[string]struct {
		cron string
		want string
	}{
		"no trigger":         {cron: "", want: ReasonNoTrigger},
		"cannot parse":       {cron: "99 * * * *", want: ReasonInvalidExpression},
		"matches no date":    {cron: "0 0 31 2 *", want: ReasonNoOccurrence},
		"perfectly ordinary": {cron: "0 3 * * *", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			tsk := Task{ID: "t", Name: "n", CronExpr: tc.cron, ProjectPath: "/repo", Enabled: true, CreatedAt: now}
			got := WithScheduleHealth([]Task{tsk}, now)[0]
			assert.Equal(t, tc.want, got.UnschedulableReason)
			assert.Equal(t, tc.want != "", got.Unschedulable,
				"the reason is present exactly when the verdict is")
			assert.Equal(t, UnschedulableReason(tsk, now), got.UnschedulableReason,
				"the record must say what the shared classifier says")
		})
	}
}
