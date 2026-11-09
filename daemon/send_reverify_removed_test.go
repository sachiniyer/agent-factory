package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// #7042: a task removed while one of its deliveries is being re-verified
// retires that watch on the first promotion that finds the task gone. Before
// the fix the refused promotion was retried on every capture until the
// 10-minute window closed, logging a WARNING each time — about 600 lines for
// one removed task — although no retry could ever apply.
func TestUnverifiedSendWatchRetiresWhenTaskRemoved(t *testing.T) {
	m, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	require.NoError(t, task.AddTask(task.Task{
		ID: "feed7042", Name: "captain-events", WatchCmd: "emit.sh",
		TargetSession: "root", ProjectPath: t.TempDir(), Enabled: true,
	}))
	attemptedAt := time.Now()
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed7042", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	// The watch fires before the send's verdict is on the record: the
	// compare-and-set refuses and the evidence banks for retry (#6592 review).
	captured(reverifyIdleRow(), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("1s"), attemptedAt.Add(2*time.Second))
	captured(reverifyBusyRow("2s"), attemptedAt.Add(3*time.Second))
	m.pausedMu.Lock()
	banked := m.unverifiedSendWatches[key]["feed7042"]
	m.pausedMu.Unlock()
	require.NotNil(t, banked, "a promotion refused before the verdict write must keep its watch")
	require.True(t, banked.fired)

	// The operator removes the task while its watch is still banked.
	require.NoError(t, task.RemoveTask("feed7042", task.ProjectExpectation{}))

	// The next capture's promotion finds the task gone: the watch retires.
	captured(reverifyIdleRow(), attemptedAt.Add(4*time.Second))
	m.pausedMu.Lock()
	retired := m.unverifiedSendWatches[key]["feed7042"]
	_, sessionStillWatched := m.unverifiedSendWatches[key]
	m.pausedMu.Unlock()
	assert.Nil(t, retired, "a removed task's watch must retire after one promotion attempt")
	assert.False(t, sessionStillWatched, "the session holds no other watch, so its map must go too")

	// Every later capture inside the window makes no attempt and logs nothing.
	for i := 0; i < 5; i++ {
		captured(reverifyBusyRow("3s"), attemptedAt.Add(time.Duration(5+i)*time.Second))
	}
	all := logs.warnings.String() + logs.info.String() + logs.errors.String()
	assert.LessOrEqual(t, len(linesMentioning(all, "feed7042")), 1,
		"a removed task's watch must log at most one line, not one per capture:\n%s", all)
}

// The other half of #7042's property: a store error that does not say the task
// is gone is no verdict about the task, so the fired watch keeps its banked
// evidence and the promotion still applies once the store answers again — the
// retry the watch has always made.
func TestUnverifiedSendWatchRetriesTransientStoreError(t *testing.T) {
	m, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst := registerStarted(t, m, repoID, repoPath, "root", session.NewFakeBackend(), true, session.Running)
	attemptedAt := time.Now()
	seedUnverifiedTask(t, "feed7043")
	key := stableSessionKey(repoID, inst)
	m.armUnverifiedSendWatch(key, inst, "feed7043", attemptedAt, task.NewTurnWatch("claude", attemptedAt))

	captured := func(content string, at time.Time) {
		m.noteUnverifiedSendEvidence(key, inst, session.Observation{Content: content, Captured: true}, at)
	}
	restore := task.SetTasksPathFnForTest(func() (string, error) {
		return "", errors.New("tasks store briefly unavailable")
	})
	captured(reverifyIdleRow(), attemptedAt.Add(time.Second))
	captured(reverifyBusyRow("1s"), attemptedAt.Add(2*time.Second))
	captured(reverifyBusyRow("2s"), attemptedAt.Add(3*time.Second))
	restore()

	m.pausedMu.Lock()
	entry := m.unverifiedSendWatches[key]["feed7043"]
	m.pausedMu.Unlock()
	require.NotNil(t, entry, "a transient store error must not retire the watch")
	require.True(t, entry.fired, "the evidence must stay banked across the failed promotion")
	require.Contains(t, logs.warnings.String(), "tasks store briefly unavailable",
		"a failed promotion that will be retried still says why")

	// The store answers again: the banked evidence promotes on the next capture.
	captured(reverifyIdleRow(), attemptedAt.Add(4*time.Second))
	stored, err := task.GetTask("feed7043")
	require.NoError(t, err)
	require.Equal(t, "sent", stored.LastRunStatus,
		"banked evidence must promote once the store recovers")
}
