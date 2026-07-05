package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// registerFailingWatcher inserts a taskWatcher directly into the supervisor's
// map with a preset delivery-failure state, bypassing newTaskWatcher (which
// would run config.RepoFromPath against a non-repo temp dir). It lets these
// tests exercise deliveryAlarms deterministically without spawning scripts.
func registerFailingWatcher(s *watcherSupervisor, id, repoID, target string, since time.Time, count int, lastErr string) *taskWatcher {
	w := &taskWatcher{
		taskID:        id,
		name:          "watch-" + id,
		repoID:        repoID,
		targetSession: target,
	}
	w.deliverFailSince = since
	w.deliverFailCount = count
	w.deliverFailErr = lastErr
	s.mu.Lock()
	s.watchers[id] = w
	s.mu.Unlock()
	return w
}

// TestDeliveryAlarms_ThresholdScopingAndClear covers the daemon projection at
// the heart of #1238 fix (c): a delivery-failure alarm is raised only once the
// failure has persisted past watcherDeliveryAlarmThreshold (so a normal ~2m
// self-heal never false-alarms), is scoped to the requesting repo, carries the
// target/pending/consecutive/error detail the banner needs, and clears the
// instant delivery succeeds.
func TestDeliveryAlarms_ThresholdScopingAndClear(t *testing.T) {
	s := newWatcherSupervisor()
	now := time.Now()

	// A failure younger than the threshold — the routine self-heal window —
	// must NOT alarm. This is the false-alarm guard.
	registerFailingWatcher(s, "recent", "repo1", "root",
		now.Add(-watcherDeliveryAlarmThreshold/2), 3, "momentary")
	require.Empty(t, s.deliveryAlarms("repo1", now),
		"a sub-threshold failure must not alarm (self-heal window)")

	// A failure aged past the threshold IS a genuine outage → alarm, with the
	// full detail the TUI banner renders.
	since := now.Add(-watcherDeliveryAlarmThreshold - time.Minute)
	w := registerFailingWatcher(s, "stale", "repo1", "root", since, 7, "target session down")

	// Back the stale watcher with a real queue so Pending reflects the backlog.
	queueDir := t.TempDir()
	w.queue = newEventQueue(queueDir, "stale")
	for i := 0; i < 5; i++ {
		require.NoError(t, w.queue.enqueue("event"))
	}

	alarms := s.deliveryAlarms("repo1", now)
	require.Len(t, alarms, 1, "a past-threshold failure must alarm")
	got := alarms[0]
	require.Equal(t, "stale", got.TaskID)
	require.Equal(t, "watch-stale", got.TaskName)
	require.Equal(t, "root", got.TargetSession)
	require.Equal(t, 7, got.Consecutive)
	require.Equal(t, 5, got.Pending, "Pending must surface the queued backlog")
	require.Equal(t, since, got.Since)
	require.Equal(t, "target session down", got.LastError)

	// Repo scoping: a failing watcher in another repo is invisible to repo1's
	// snapshot, but the all-repos read (empty repoID) sees both.
	registerFailingWatcher(s, "otherrepo", "repo2", "root",
		now.Add(-watcherDeliveryAlarmThreshold-time.Minute), 4, "x")
	require.Len(t, s.deliveryAlarms("repo1", now), 1, "another repo's alarm must not leak into repo1")
	require.Len(t, s.deliveryAlarms("", now), 2, "the all-repos read sees every failing target")

	// Recovery clears the alarm the moment a delivery succeeds.
	w.recordDeliveryResult(now, nil)
	require.Empty(t, s.deliveryAlarms("repo1", now),
		"a successful delivery must clear the alarm")
}

// TestRecordDeliveryResult_FailureRunLifecycle pins the failure-run bookkeeping
// recordDeliveryResult maintains: the first failure stamps deliverFailSince,
// subsequent failures extend the same run (since unchanged, count climbs, error
// refreshed), and one success zeroes the whole run so a later failure starts a
// fresh window.
func TestRecordDeliveryResult_FailureRunLifecycle(t *testing.T) {
	w := &taskWatcher{taskID: "aaaa", name: "watch-aaaa"}
	t0 := time.Now()

	w.recordDeliveryResult(t0, errors.New("first"))
	require.Equal(t, t0, w.deliverFailSince, "first failure stamps the run start")
	require.Equal(t, 1, w.deliverFailCount)
	require.Equal(t, "first", w.deliverFailErr)

	w.recordDeliveryResult(t0.Add(time.Minute), errors.New("second"))
	require.Equal(t, t0, w.deliverFailSince, "a continuing failure keeps the original run start")
	require.Equal(t, 2, w.deliverFailCount)
	require.Equal(t, "second", w.deliverFailErr, "the latest error is retained")

	w.recordDeliveryResult(t0.Add(2*time.Minute), nil)
	require.True(t, w.deliverFailSince.IsZero(), "a success clears the run start")
	require.Equal(t, 0, w.deliverFailCount)
	require.Empty(t, w.deliverFailErr)

	// A later failure opens a brand-new window at its own timestamp.
	t1 := t0.Add(10 * time.Minute)
	w.recordDeliveryResult(t1, errors.New("again"))
	require.Equal(t, t1, w.deliverFailSince)
	require.Equal(t, 1, w.deliverFailCount)
}

// TestControlServerSnapshot_ProjectsDeliveryAlarms proves the alarm rides the
// authoritative Snapshot RPC — a field on the response, not a side channel
// (#1238): the controlServer folds the supervisor's persistent delivery
// failures into resp.DeliveryAlarms, scoped to the request's repo.
func TestControlServerSnapshot_ProjectsDeliveryAlarms(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	s := newWatcherSupervisor()
	cs := &controlServer{manager: m, watchers: s}

	repoID := config.RepoIDFromRoot("/tmp/alarm-repo")

	// Healthy steady state: the snapshot carries no alarms.
	var resp SnapshotResponse
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: repoID}, &resp))
	require.Empty(t, resp.DeliveryAlarms, "no alarms when nothing is failing")

	// A past-threshold failing watcher surfaces on the response for its repo.
	registerFailingWatcher(s, "stale", repoID, "root",
		time.Now().Add(-watcherDeliveryAlarmThreshold-time.Minute), 9, "target down")
	resp = SnapshotResponse{}
	require.NoError(t, cs.Snapshot(SnapshotRequest{RepoID: repoID}, &resp))
	require.Len(t, resp.DeliveryAlarms, 1, "the persistent failure must project onto the snapshot")
	require.Equal(t, "root", resp.DeliveryAlarms[0].TargetSession)
	require.Equal(t, 9, resp.DeliveryAlarms[0].Consecutive)
}
