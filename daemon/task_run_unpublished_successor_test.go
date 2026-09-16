package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover a task row that still names an earlier run after a later
// run's start publication failed in both BeginTaskRun and the post-RPC
// UpdateTaskRunStart. Leaving out the BeginTaskRun call for the later run
// models that double failure.

// A row that names the interrupted run must still check for a committed later
// run before it records the interruption. The later run may be visible only on
// disk (it failed to load) or only in memory. The check must find it either way.
func TestIdentifiedInterruptionDefersToCommittedUnpublishedSuccessor(t *testing.T) {
	for _, visibility := range []string{"persisted", "registered"} {
		t.Run(visibility, func(t *testing.T) {
			manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
			tsk := addStatusTestTask(t, enabledCronTask("succ0001", repoPath))
			olderAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
			older := publishedTaskRun(t, repoPath, tsk, "older-run", 1, olderAt)

			newerAt := olderAt.Add(time.Minute)
			newer := newTaskRunInstance(t, repoPath, tsk, "newer-run", 2, newerAt)
			stored := []session.InstanceData{older.ToInstanceData()}
			if visibility == "persisted" {
				stored = append(stored, newer.ToInstanceData())
			}
			seedTaskRunStore(t, repoID, stored...)
			olderKey := daemonInstanceKey(repoID, older.Title)
			manager.mu.Lock()
			manager.instances[olderKey] = older
			if visibility == "registered" {
				manager.instances[daemonInstanceKey(repoID, newer.Title)] = newer
			}
			manager.mu.Unlock()

			interruptTaskRunRuntime(t, manager, repoID, olderKey, older)

			got, err := task.GetTask(tsk.ID)
			require.NoError(t, err)
			assert.Equal(t, task.RunStatusStarted, got.LastRunStatus,
				"the earlier run's interruption must not become the status of a task whose newest run is still live")
			assert.Equal(t, older.ID, got.LastRunSessionID)
			assert.Contains(t, logs.warnings.String(), "a later run of this task exists")
			_, pending := older.PendingTaskRunInterruption()
			assert.False(t, pending, "a refused outcome is settled, not left in the outbox")
			manager.mu.Lock()
			_, owed := manager.settleOwed[stableSessionKey(repoID, older)]
			manager.mu.Unlock()
			assert.False(t, owed, "a refused outcome must not be retried forever")

			// The later run can still record its own outcome, because the row was
			// left in a status its claim accepts.
			_, applied, err := manager.claimUnidentifiedInterruptedTaskRun(repoID, newer.TaskRun())
			require.NoError(t, err)
			require.True(t, applied, "the later run must still be able to record its own interruption")
			got, err = task.GetTask(tsk.ID)
			require.NoError(t, err)
			assert.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
			assert.Equal(t, newer.ID, got.LastRunSessionID)
			assert.Equal(t, uint64(2), got.LastRunSequence)
		})
	}
}

// A pending create has not tried to publish its start yet. Counting it as a
// later run would lose this outcome for good if the create never commits. When
// the create does publish, its greater sequence replaces the outcome anyway.
func TestIdentifiedInterruptionIsRecordedWhileALaterCreateIsPending(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("succ0002", repoPath))
	olderAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	older := publishedTaskRun(t, repoPath, tsk, "older-run", 1, olderAt)
	seedTaskRunStore(t, repoID, older.ToInstanceData())
	olderKey := daemonInstanceKey(repoID, older.Title)
	newerAt := olderAt.Add(time.Minute)
	manager.mu.Lock()
	manager.instances[olderKey] = older
	manager.pendingCreates[daemonInstanceKey(repoID, "pending-run")] = session.InstanceData{
		ID: "pending-run-id", Title: "pending-run", Path: repoPath,
		TaskID: tsk.ID, TaskGenerationID: tsk.GenerationID, TaskRunActive: true,
		TaskRunSequence: 2, TaskRunAt: newerAt, CreatedAt: newerAt,
		Status: session.Loading, InFlightOp: session.OpCreating,
	}
	manager.mu.Unlock()

	interruptTaskRunRuntime(t, manager, repoID, olderKey, older)

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus,
		"a create that may never commit must not suppress this run's recorded interruption")
	require.Equal(t, older.ID, got.LastRunSessionID)

	_, applied, err := task.BeginTaskRun(
		tsk.ID, tsk.GenerationID, "pending-run-id", 2, 0, newerAt, task.RunStatusStarted)
	require.NoError(t, err)
	require.True(t, applied, "the pending create's publication must still replace the earlier outcome")
}

// The reverse order: the earlier run's interruption lands first, and then a
// later run is admitted and both of its start writes fail. The earlier outcome
// must not stop the later run from recording its own interruption.
func TestLaterRunInterruptionReplacesEarlierRunsRecordedInterruption(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("succ0003", repoPath))
	olderAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	older := publishedTaskRun(t, repoPath, tsk, "older-run", 1, olderAt)
	seedTaskRunStore(t, repoID, older.ToInstanceData())
	olderKey := daemonInstanceKey(repoID, older.Title)
	manager.mu.Lock()
	manager.instances[olderKey] = older
	manager.mu.Unlock()
	interruptTaskRunRuntime(t, manager, repoID, olderKey, older)
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
	require.Equal(t, older.ID, got.LastRunSessionID)

	newerAt := olderAt.Add(time.Minute)
	newer := newTaskRunInstance(t, repoPath, tsk, "newer-run", 2, newerAt)
	require.NoError(t, appendInstanceData(repoID, newer.ToInstanceData()))
	newerKey := daemonInstanceKey(repoID, newer.Title)
	manager.mu.Lock()
	manager.instances[newerKey] = newer
	manager.mu.Unlock()
	interruptTaskRunRuntime(t, manager, repoID, newerKey, newer)

	got, err = task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
	assert.Equal(t, newer.ID, got.LastRunSessionID,
		"the later run's outcome must replace the earlier run's recorded interruption")
	assert.Equal(t, uint64(2), got.LastRunSequence)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(newerAt))
}

// The replacement allowed above covers only a recorded interruption. A watcher
// supervision status on the row stays, even when the row names an earlier run.
func TestLaterRunInterruptionPreservesSupervisionStatusOnEarlierRun(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("succ0004", repoPath))
	olderAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	older := publishedTaskRun(t, repoPath, tsk, "older-run", 1, olderAt)
	_, err := task.UpdateTaskStatus(tsk.ID, nil, "errored: watcher exited")
	require.NoError(t, err)
	newer := newTaskRunInstance(t, repoPath, tsk, "newer-run", 2, olderAt.Add(time.Minute))
	seedTaskRunStore(t, repoID, older.ToInstanceData(), newer.ToInstanceData())

	_, applied, err := manager.claimUnidentifiedInterruptedTaskRun(repoID, newer.TaskRun())
	require.NoError(t, err)
	require.False(t, applied)
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, "errored: watcher exited", got.LastRunStatus)
	assert.Equal(t, older.ID, got.LastRunSessionID)
}

// If the later-run check cannot read the session stores, it fails closed. The
// outcome stays owed and lands once the stores can be read again.
func TestIdentifiedInterruptionRetriesWhileSessionStoresAreUnreadable(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("succ0005", repoPath))
	runAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	inst := publishedTaskRun(t, repoPath, tsk, "unreadable-stores", 1, runAt)
	seedTaskRunStore(t, repoID, inst.ToInstanceData())
	key := daemonInstanceKey(repoID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	previous := loadPersistedTaskRunsForAttribution
	loadPersistedTaskRunsForAttribution = func(string) ([]session.InstanceData, error) {
		return nil, assert.AnError
	}
	t.Cleanup(func() { loadPersistedTaskRunsForAttribution = previous })

	interruptTaskRunRuntime(t, manager, repoID, key, inst)
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, task.RunStatusStarted, got.LastRunStatus,
		"an unproven absence of a later run must not record the outcome")
	require.Contains(t, logs.warnings.String(), "will retry")
	manager.mu.Lock()
	entry, owed := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	require.True(t, owed)
	require.NotNil(t, entry.interruptedTaskRun)

	loadPersistedTaskRunsForAttribution = previous
	require.NoError(t, inst.Transition(session.ConfirmRuntimeReplacementLive()))
	manager.FlushOwedSettlements()
	got, err = task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
	require.Equal(t, inst.ID, got.LastRunSessionID)
}

// A row that names this run but already refuses the write (here, because a
// watcher status replaced the active one) must not read the session stores. An
// unrelated unreadable store must not turn that refusal into a retry loop.
func TestRefusingIdentifiedRowDoesNotScanUnreadableSessionStores(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("succ0006", repoPath))
	runAt := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	inst := publishedTaskRun(t, repoPath, tsk, "refusing-row", 1, runAt)
	_, err := task.UpdateTaskStatus(tsk.ID, nil, "errored: watcher exited")
	require.NoError(t, err)

	previous := loadPersistedTaskRunsForAttribution
	loadPersistedTaskRunsForAttribution = func(string) ([]session.InstanceData, error) {
		return nil, assert.AnError
	}
	t.Cleanup(func() { loadPersistedTaskRunsForAttribution = previous })

	require.NoError(t, manager.recordInterruptedTaskRun(
		repoID, daemonInstanceKey(repoID, inst.Title), inst, inst.TaskRun()))
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, "errored: watcher exited", got.LastRunStatus)
}

func newTaskRunInstance(
	t *testing.T,
	repoPath string,
	tsk task.Task,
	title string,
	sequence uint64,
	runAt time.Time,
) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: sequence,
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	return inst
}

// publishedTaskRun is newTaskRunInstance plus a successful start publication,
// so the task row names the returned run.
func publishedTaskRun(
	t *testing.T,
	repoPath string,
	tsk task.Task,
	title string,
	sequence uint64,
	runAt time.Time,
) *session.Instance {
	t.Helper()
	inst := newTaskRunInstance(t, repoPath, tsk, title, sequence, runAt)
	_, applied, err := task.BeginTaskRun(
		tsk.ID, tsk.GenerationID, inst.ID, sequence, 0, runAt, task.RunStatusStarted)
	require.NoError(t, err)
	require.True(t, applied)
	return inst
}

func seedTaskRunStore(t *testing.T, repoID string, rows ...session.InstanceData) {
	t.Helper()
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, raw))
}

func interruptTaskRunRuntime(t *testing.T, manager *Manager, repoID, key string, inst *session.Instance) {
	t.Helper()
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))
	require.False(t, inst.TaskRunActive(), "the session-side close is one-way")
}
