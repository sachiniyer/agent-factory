package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestReconcileSeedsReplacementFromPostFlushDropTotal(t *testing.T) {
	dir := t.TempDir()
	original := watchTask("d4357005", "sleep 60", dir)
	replacement := original
	replacement.Name = "replacement"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	old := s.newTaskWatcher(original)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.reconcile([]task.Task{replacement}, []task.Task{replacement}, everyWatchTask()))
	got := s.watcherInstance(replacement.ID)
	require.NotNil(t, got)
	require.NotSame(t, old, got)
	require.Equal(t, 7, got.dropped,
		"the replacement must inherit drops flushed after reconcile's task snapshot")
	require.Equal(t, 7, got.persistedDrops)
}

func TestExplicitRestartSeedsReplacementFromPostFlushDropTotal(t *testing.T) {
	dir := t.TempDir()
	tsk := watchTask("d4357006", "sleep 60", dir)
	tsk.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(tsk))
	old := s.newTaskWatcher(tsk)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.restart(tsk))
	got := s.watcherInstance(tsk.ID)
	require.NotNil(t, got)
	require.NotSame(t, old, got)
	require.Equal(t, 7, got.dropped,
		"the explicit restart must inherit drops flushed after its task snapshot")
	require.Equal(t, 7, got.persistedDrops)
}
