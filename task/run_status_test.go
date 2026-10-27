package task

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// op is one run-status write: a keyed write by run k, or an unkeyed one (k zero).
type runOp struct {
	k      RunKey
	status string
	at     time.Time
}

// TestRecordRunStatusInterleavings enumerates the interleavings behind #5645,
// #5646, #5661 and #5662 against the one run-status state machine and asserts
// the final status of each. Every scenario is then replayed with each
// already-applied write re-delivered after every step — the effect of a daemon
// restart re-arming a pending stamp — and must end identically: re-delivery of
// a settled write is never able to change the outcome.
func TestRecordRunStatusInterleavings(t *testing.T) {
	base := time.Date(2026, time.October, 27, 12, 0, 0, 0, time.UTC)
	k1 := RunKey{ID: "run-1", StartedAt: base}
	k2 := RunKey{ID: "run-2", StartedAt: base.Add(time.Hour)}
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	failed := RunFailedUndeliveredStatus

	cases := []struct {
		name string
		ops  []runOp
		want string
	}{
		{"#5645: failure stamped before the same run's started", []runOp{
			{k1, failed, at(1)}, {k1, RunStartedStatus, at(2)}}, failed},
		{"#5645: started then failure", []runOp{
			{k1, RunStartedStatus, at(1)}, {k1, failed, at(2)}}, failed},
		{"#5645: stale failure after a newer run started", []runOp{
			{k1, RunStartedStatus, at(1)}, {k2, RunStartedStatus, at(61)}, {k1, failed, at(62)}}, RunStartedStatus},
		{"#5646: failure then restart re-stamp is idempotent", []runOp{
			{k1, RunStartedStatus, at(1)}, {k1, failed, at(2)}, {k1, failed, at(3)}}, failed},
		{"#5661: previous run's failure lands during next delivery startup", []runOp{
			{k1, RunStartedStatus, at(1)}, {k1, failed, at(60)}, {k2, RunStartedStatus, at(61)}}, RunStartedStatus},
		{"#5661: previous failure lands after the next run's started", []runOp{
			{k1, RunStartedStatus, at(1)}, {k2, RunStartedStatus, at(61)}, {k1, failed, at(62)}}, RunStartedStatus},
		{"#5662: restart re-arms a settled failure over a good newer run", []runOp{
			{k1, failed, at(1)}, {k2, RunStartedStatus, at(61)}, {k2, "sent", at(62)}, {k1, failed, at(63)}}, "sent"},
		{"newer run's failure supersedes an older run", []runOp{
			{k1, RunStartedStatus, at(1)}, {k2, RunStartedStatus, at(61)}, {k2, failed, at(62)}}, failed},
		{"started never regresses a settled run", []runOp{
			{k1, RunStartedStatus, at(1)}, {k1, "sent", at(2)}, {k1, RunStartedStatus, at(3)}}, "sent"},
		{"unkeyed write advances the clock past an older run", []runOp{
			{k1, RunStartedStatus, at(1)}, {RunKey{}, "errored: boom", at(120)}, {k1, failed, at(121)}}, "errored: boom"},
		{"legacy row without a key accepts a keyed writer", []runOp{
			{RunKey{}, "stopped", time.Time{}}, {k1, failed, at(1)}}, failed},
	}

	run := func(t *testing.T, ops []runOp, replay bool) string {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		require.NoError(t, AddTask(Task{ID: "f9a0b1c2", Name: "t", CronExpr: "0 * * * *", Prompt: "p", ProjectPath: "/tmp", Enabled: true}))
		apply := func(o runOp) {
			var ts *time.Time
			if !o.at.IsZero() {
				a := o.at
				ts = &a
			}
			_, _, err := RecordRunStatus("f9a0b1c2", o.k, ts, o.status)
			require.NoError(t, err)
		}
		for i, o := range ops {
			apply(o)
			if replay {
				// A restart between any two steps: every write so far is re-delivered.
				for _, prev := range ops[:i+1] {
					if !prev.k.IsZero() {
						apply(prev)
					}
				}
			}
		}
		got, err := GetTask("f9a0b1c2")
		require.NoError(t, err)
		return got.LastRunStatus
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, run(t, c.ops, false), "straight-line")
		})
		t.Run(c.name+" (re-delivered after every step)", func(t *testing.T) {
			assert.Equal(t, c.want, run(t, c.ops, true), "re-delivery must not change the outcome")
		})
	}
}

// A settled failure re-delivered after a restart must not rewrite LastRunAt.
func TestRecordRunStatusReplayKeepsLastRunAt(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, AddTask(Task{ID: "f9a0b1c2", Name: "t", CronExpr: "0 * * * *", Prompt: "p", ProjectPath: "/tmp", Enabled: true}))
	k := RunKey{ID: "run-1", StartedAt: time.Now().Add(-time.Hour)}
	first := time.Now().Add(-time.Minute).Truncate(time.Second)
	_, applied, err := RecordRunStatus("f9a0b1c2", k, &first, RunFailedUndeliveredStatus)
	require.NoError(t, err)
	require.True(t, applied)
	later := first.Add(time.Hour)
	_, applied, err = RecordRunStatus("f9a0b1c2", k, &later, RunFailedUndeliveredStatus)
	require.NoError(t, err)
	assert.False(t, applied)
	got, err := GetTask("f9a0b1c2")
	require.NoError(t, err)
	assert.True(t, got.LastRunAt.Equal(first))
}
