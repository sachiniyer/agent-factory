package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// farOutNow is the day #4843 was decided, so the fixture reads like the report.
var farOutNow = time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)

func farOutTask(next time.Duration) Task {
	at := farOutNow.Add(next)
	return Task{ID: "f77f4e99", CronExpr: "0 7 21 9 *", Enabled: true, NextRunAt: &at}
}

func TestNextRunFarOut_Boundary(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name string
		next time.Duration
		want bool
	}{
		{"59 days", 59 * day, false},
		{"exactly 60 days", 60 * day, false},
		{"one second past 60 days", 60*day + time.Second, true},
		{"61 days", 61 * day, true},
		{"a year", 362 * day, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NextRunFarOut(farOutTask(tc.next), farOutNow))
		})
	}
}

func TestNextRunFarOut_NeverFlagsDisabledWatchOrUnarmed(t *testing.T) {
	disabled := farOutTask(362 * 24 * time.Hour)
	disabled.Enabled = false
	assert.False(t, NextRunFarOut(disabled, farOutNow), "a disabled task is not expected to fire")

	watch := Task{ID: "w", WatchCmd: "tail -f ci.log", Enabled: true}
	assert.False(t, NextRunFarOut(watch, farOutNow), "a watch task has no next run")

	unarmed := farOutTask(0)
	unarmed.NextRunAt = nil
	assert.False(t, NextRunFarOut(unarmed, farOutNow),
		"no live next run means nothing to flag — the cron expression is never re-parsed")
	assert.Empty(t, FarOutNote(unarmed, farOutNow))
}

func TestFarOutNote_WordsTheReportedTask(t *testing.T) {
	next := time.Date(2027, time.September, 21, 7, 0, 0, 0, time.UTC)
	tsk := Task{ID: "f77f4e99", CronExpr: "0 7 21 9 *", Enabled: true, NextRunAt: &next}
	assert.Equal(t, "2027-09-21 (in 11 months)", FarOutNote(tsk, farOutNow))

	near := farOutTask(30 * 24 * time.Hour)
	assert.Empty(t, FarOutNote(near, farOutNow))
}

func TestFarOutDistance_WholeMonthsRoundedDown(t *testing.T) {
	cases := []struct {
		next time.Time
		want string
	}{
		{time.Date(2026, time.November, 24, 12, 0, 0, 0, time.UTC), "in 2 months"},
		{time.Date(2026, time.November, 24, 11, 59, 0, 0, time.UTC), "in 1 month"},
		{time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC), "in 1 month"},
		{time.Date(2027, time.September, 24, 12, 0, 0, 0, time.UTC), "in 12 months"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, FarOutDistance(tc.next, farOutNow), tc.next.String())
	}
}

// TestNextRunFar_JSONFieldIsAdditive pins the wire name and that it is absent
// (not false) when the run is near — existing readers see nothing new unless
// the task is flagged, and no existing field moves.
func TestNextRunFar_JSONFieldIsAdditive(t *testing.T) {
	far := StampNextRunFar([]Task{farOutTask(362 * 24 * time.Hour)}, farOutNow)[0]
	data, err := json.Marshal(far)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"next_run_far":true`)
	assert.Contains(t, string(data), `"next_run_at":`)

	near := StampNextRunFar([]Task{farOutTask(24 * time.Hour)}, farOutNow)[0]
	data, err = json.Marshal(near)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(data), "next_run_far"))
}

// TestNextRunFar_IsDerivedNeverPersisted: like NextRunAt, the flag is a claim
// about an instant and must not survive a disk read or reach disk.
func TestNextRunFar_IsDerivedNeverPersisted(t *testing.T) {
	tsk := StampNextRunFar([]Task{farOutTask(362 * 24 * time.Hour)}, farOutNow)[0]
	require.True(t, tsk.NextRunFar)
	tsk.stripDerived()
	assert.False(t, tsk.NextRunFar)

	read := WithScheduleHealth([]Task{farOutTask(362 * 24 * time.Hour)}, farOutNow)
	read[0].NextRunFar = true
	read = WithScheduleHealth(read, farOutNow)
	assert.False(t, read[0].NextRunFar, "a disk read observed no next run, so nothing is far out")
}

// TestApplyLiveArming_CarriesNextRunFar: the TUI rail adopts the daemon's
// observation onto disk rows, and the flag travels with the NextRunAt it was
// derived from — never one without the other.
func TestApplyLiveArming_CarriesNextRunFar(t *testing.T) {
	next := farOutNow.Add(362 * 24 * time.Hour)
	observed := armingFixture(1, "a", "0 7 21 9 *")
	observed.Arming, observed.NextRunAt, observed.NextRunFar = ArmingArmed, &next, true

	got := ApplyLiveArming([]Task{armingFixture(1, "a", "0 7 21 9 *")}, []Task{observed})
	require.NotNil(t, got[0].NextRunAt)
	assert.True(t, got[0].NextRunFar)
}
