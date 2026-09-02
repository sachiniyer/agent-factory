package task

import (
	"encoding/json"
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
		assert.False(t, DeriveScheduleHealth(tsk, now).Overdue,
			"nothing can be derived from an expression with no occurrences; the arming state reports this task instead")
	})
	t.Run("expression that matches no date", func(t *testing.T) {
		// February 31st: legal syntax, never occurs. Next() gives up after five
		// years and returns the zero time.
		tsk := cronTask("0 0 31 2 *", &long)
		assert.False(t, DeriveScheduleHealth(tsk, now).Overdue)
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
	tsk := cronTask("20 * * * *", &last)
	tsk.ID = "persist1"
	// A caller handing the store a fully-annotated record — exactly what a
	// load-modify-save path would do now that every load annotates.
	next := at(2026, time.September, 1, 15, 20, 0)
	tsk.Overdue, tsk.MissedOccurrences, tsk.Arming, tsk.NextRunAt = true, 432, ArmingArmed, &next
	require.NoError(t, AddTask(tsk))

	raw, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	require.NoError(t, err)
	for _, key := range []string{"overdue", "missed_occurrences", "next_run_at", "arming"} {
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
