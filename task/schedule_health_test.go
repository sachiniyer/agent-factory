package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWithScheduleHealthUnknownClearsEveryObservedField(t *testing.T) {
	next := time.Now().Add(time.Hour)
	in := Task{
		ID: "t", Name: "keep", CronExpr: "*/5 * * * *", Enabled: true,
		Arming: ArmingArmed, NextRunAt: &next, NextRunFar: true, Overdue: true,
		MissedOccurrences: 2, MissedOccurrencesCapped: true,
		Unschedulable: true, UnschedulableReason: "x",
	}
	got := in.WithScheduleHealthUnknown()
	require.Equal(t, ArmingUnknown, got.Arming)
	require.Nil(t, got.NextRunAt)
	require.False(t, got.NextRunFar)
	require.False(t, got.Overdue)
	require.Zero(t, got.MissedOccurrences)
	require.False(t, got.MissedOccurrencesCapped)
	require.False(t, got.Unschedulable)
	require.Empty(t, got.UnschedulableReason)
	require.True(t, got.Unassessable)
	// The definition is untouched, and the receiver is not mutated.
	require.Equal(t, "keep", got.Name)
	require.Equal(t, "*/5 * * * *", got.CronExpr)
	require.NotNil(t, in.NextRunAt, "a value receiver: the caller's copy keeps its observations")
}
