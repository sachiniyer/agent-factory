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

func TestReconcileDoesNotSeedAReusedIDFromItsPredecessorGeneration(t *testing.T) {
	dir := t.TempDir()
	predecessor := watchTask("d4357007", "sleep 60", dir)
	replacement := predecessor
	replacement.GenerationID = "rebound-generation"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	old := s.newTaskWatcher(predecessor)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.reconcile([]task.Task{replacement}, []task.Task{replacement}, everyWatchTask()))
	got := s.watcherInstance(replacement.ID)
	require.NotNil(t, got)
	require.NotSame(t, old, got)
	require.Zero(t, got.dropped,
		"a flushed total belongs to the incarnation that earned it, not to the reused ID")
	require.Zero(t, got.persistedDrops)
}

func TestExplicitRestartDoesNotSeedAReusedIDFromItsPredecessorGeneration(t *testing.T) {
	dir := t.TempDir()
	predecessor := watchTask("d4357008", "sleep 60", dir)
	replacement := predecessor
	replacement.GenerationID = "rebound-generation"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	old := s.newTaskWatcher(predecessor)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.restart(replacement))
	got := s.watcherInstance(replacement.ID)
	require.NotNil(t, got)
	require.NotSame(t, old, got)
	require.Zero(t, got.dropped,
		"the explicit restart must not transfer drops across task incarnations")
	require.Zero(t, got.persistedDrops)
}

func TestReconcilePersistentlyClearsAReboundRowDropSeed(t *testing.T) {
	dir := t.TempDir()
	predecessor := watchTask("d4357010", "sleep 60", dir)
	replacement := predecessor
	replacement.GenerationID = "rebound-generation"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	var resets []string
	s.resetDrops = func(taskID, generationID string) error {
		resets = append(resets, taskID+"/"+generationID)
		return nil
	}
	old := s.newTaskWatcher(predecessor)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.reconcile([]task.Task{replacement}, []task.Task{replacement}, everyWatchTask()))
	require.Equal(t, []string{replacement.ID + "/rebound-generation"}, resets,
		"the rebound must clear the durable row's inherited count for the generation that now owns the ID")
}

func TestReconcileSkipsDropResetOnSameGenerationReplacement(t *testing.T) {
	dir := t.TempDir()
	original := watchTask("d4357011", "sleep 60", dir)
	replacement := original
	replacement.Name = "replacement"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	resetCalled := false
	s.resetDrops = func(taskID, generationID string) error {
		resetCalled = true
		return nil
	}
	old := s.newTaskWatcher(original)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.reconcile([]task.Task{replacement}, []task.Task{replacement}, everyWatchTask()))
	require.False(t, resetCalled,
		"an ordinary same-generation replacement has no predecessor evidence to clear")
}

func TestExplicitRestartPersistentlyClearsAReboundRowDropSeed(t *testing.T) {
	dir := t.TempDir()
	predecessor := watchTask("d4357012", "sleep 60", dir)
	replacement := predecessor
	replacement.GenerationID = "rebound-generation"
	replacement.DroppedEvents = 2

	s, _ := newTestSupervisor(t, staticTasks(replacement))
	var resets []string
	s.resetDrops = func(taskID, generationID string) error {
		resets = append(resets, taskID+"/"+generationID)
		return nil
	}
	old := s.newTaskWatcher(predecessor)
	old.dropped = 7
	old.persistedDrops = 2
	old.lastDroppedAt = time.Now()
	close(old.doneCh)
	s.watchers[old.taskID] = old

	require.NoError(t, s.restart(replacement))
	require.Equal(t, []string{replacement.ID + "/rebound-generation"}, resets,
		"the explicit restart's rebound must clear the durable row's inherited count too")
}
