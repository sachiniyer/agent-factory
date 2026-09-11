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

func TestRestoredTaskRuntimeIsRecordedInterruptedAndSkipsOnComplete(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0001", repoPath)
	tsk.OnComplete = task.OnCompleteArchive
	tsk = addStatusTestTask(t, tsk)

	deliveredAt := time.Date(2026, 9, 11, 9, 0, 0, 123, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "interrupted-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: deliveredAt, TaskRunAt: deliveredAt,
		TaskRunSequence: 1, TaskRunRevision: 0,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, deliveredAt, "started")
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))
	require.False(t, inst.TaskRunActive(),
		"the pre-live settlement must close the run before the restore fence drops")
	assert.False(t, persistedInstanceByTitle(t, repoID, inst.Title).TaskRunActive,
		"a crash before the post-recovery write must not reload the interrupted run as active")
	require.NoError(t, inst.Transition(session.ConfirmLive()))
	require.False(t, inst.TaskRunActive(),
		"the replacement runtime did not receive the prompt and cannot own the interrupted run")

	gotTask, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, TaskStatusInterrupted, gotTask.LastRunStatus)
	assert.Equal(t, inst.ID, gotTask.LastRunSessionID)
	require.NotNil(t, gotTask.LastRunAt)
	assert.True(t, gotTask.LastRunAt.Equal(deliveredAt),
		"recording interruption must retain the original delivery time")
	assert.Contains(t, logs.warnings.String(), "restored it without replaying the prompt")
	assert.Contains(t, logs.warnings.String(), "skipped on_complete")
	assert.Contains(t, logs.warnings.String(), "left the session in place for inspection")

	archiveCalled := make(chan ArchiveSessionRequest, 1)
	previousArchive := archiveSessionForLifecycle
	archiveSessionForLifecycle = func(_ *Manager, req ArchiveSessionRequest, _ sessionTeardownGuard) error {
		archiveCalled <- req
		return nil
	}
	t.Cleanup(func() { archiveSessionForLifecycle = previousArchive })

	was := inst.TaskRunActive()
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, inst, was)
	select {
	case <-archiveCalled:
		t.Fatal("the replacement runtime's idle state must not apply on_complete to interrupted work")
	default:
	}
	manager.mu.Lock()
	registered := manager.instances[key] == inst
	manager.mu.Unlock()
	assert.True(t, registered, "the interrupted session must remain available for inspection")

	// Preserve the other half of the boundary: the original runtime that did
	// receive its prompt still completes on its own idle edge and gets the task's
	// declared lifecycle.
	normal, err := session.NewInstance(session.InstanceOptions{
		Title: "completed-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
	})
	require.NoError(t, err)
	normal.SetStartedForTest(true)
	normal.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, normal.Title)] = normal
	manager.mu.Unlock()
	was = normal.TaskRunActive()
	require.NoError(t, normal.Transition(session.ObserveLiveness(session.LiveReady)))
	manager.applyTaskSessionLifecycleOnRunEnd(repoID, normal, was)
	select {
	case req := <-archiveCalled:
		assert.Equal(t, normal.ID, req.ID)
		assert.Equal(t, normal.Title, req.Title)
	case <-time.After(5 * time.Second):
		t.Fatal("a prompted runtime's normal idle edge must still apply on_complete")
	}
}

func TestRestoredOlderTaskRuntimeDoesNotOverwriteNewerRunStatus(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0002", repoPath)
	tsk = addStatusTestTask(t, tsk)

	olderRunAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "older-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: olderRunAt, TaskRunAt: olderRunAt,
		TaskRunSequence: 1, TaskRunRevision: 0,
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	newerRunAt := olderRunAt.Add(time.Minute)
	_, _, err = task.BeginTaskRun(
		tsk.ID, tsk.GenerationID, "successor-session", 2, 0, newerRunAt, "started")
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))

	gotTask, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.NotNil(t, gotTask.LastRunAt)
	assert.True(t, gotTask.LastRunAt.Equal(newerRunAt))
	assert.Equal(t, "started", gotTask.LastRunStatus,
		"an older session's interruption must not replace the newer run's status")
	assert.Equal(t, "successor-session", gotTask.LastRunSessionID)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredTaskRuntimeOutcomeWinsRaceWithStartedStatus(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0003", repoPath)
	previousRunAt := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	tsk.LastRunAt = &previousRunAt
	tsk.LastRunStatus = "started"
	tsk = addStatusTestTask(t, tsk)
	_, err := task.UpdateTaskStatus(tsk.ID, &previousRunAt, "started")
	require.NoError(t, err)

	runAt := previousRunAt.Add(time.Hour)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "publication-race", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1, TaskRunRevision: 1,
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))

	_, applied, err := task.UpdateTaskRunStart(
		tsk.ID, tsk.GenerationID, inst.ID, 1, 1, runAt, "started")
	require.NoError(t, err)
	assert.False(t, applied, "the delayed delivery writer must not reopen the interrupted outcome")
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(runAt))
	assert.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
	assert.Equal(t, inst.ID, got.LastRunSessionID)
}

func TestRestoredLegacyTaskRuntimeUsesAttributedLastRunTimestamp(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0004", repoPath)
	createdAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	legacyRunAt := createdAt.Add(3 * time.Second)
	tsk.LastRunAt = &legacyRunAt
	tsk.LastRunStatus = "started"
	tsk = addStatusTestTask(t, tsk)
	_, err := task.UpdateTaskStatus(tsk.ID, &legacyRunAt, "started")
	require.NoError(t, err)

	// No TaskRunAt: this is the shape persisted by binaries before explicit run
	// identity. Their task caller minted LastRunAt only after CreateSession returned.
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "legacy-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: createdAt,
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(legacyRunAt), "legacy attribution must preserve the stored delivery time")
	assert.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
	assert.Equal(t, inst.ID, got.LastRunSessionID)
}

func TestRestoredLegacyTaskRuntimeDoesNotClaimKnownSuccessor(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0005", repoPath)
	createdAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	newerRunAt := createdAt.Add(time.Minute)
	tsk.LastRunAt = &newerRunAt
	tsk.LastRunStatus = "started"
	tsk = addStatusTestTask(t, tsk)
	_, err := task.UpdateTaskStatus(tsk.ID, &newerRunAt, "started")
	require.NoError(t, err)

	legacy, err := session.NewInstance(session.InstanceOptions{
		Title: "legacy-older", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: createdAt,
	})
	require.NoError(t, err)
	legacy.SetStartedForTest(true)
	legacy.SetStatusForTest(session.Running)
	successor, err := session.NewInstance(session.InstanceOptions{
		Title: "known-successor", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: newerRunAt,
	})
	require.NoError(t, err)
	key := daemonInstanceKey(repoID, legacy.Title)
	seedDiskInstance(t, repoID, legacy.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = legacy
	manager.instances[daemonInstanceKey(repoID, successor.Title)] = successor
	manager.mu.Unlock()

	require.NoError(t, legacy.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, legacy.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, legacy))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, "started", got.LastRunStatus)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredLegacyTaskRuntimeDoesNotReplaceTerminalOutcome(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0006", repoPath)
	createdAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	terminalRunAt := createdAt.Add(time.Minute)
	tsk.LastRunAt = &terminalRunAt
	tsk.LastRunStatus = "errored: successor failed"
	tsk = addStatusTestTask(t, tsk)
	_, err := task.UpdateTaskStatus(tsk.ID, &terminalRunAt, "errored: successor failed")
	require.NoError(t, err)

	legacy, err := session.NewInstance(session.InstanceOptions{
		Title: "legacy-before-terminal", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: createdAt,
	})
	require.NoError(t, err)
	legacy.SetStartedForTest(true)
	legacy.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, legacy.Title)
	seedDiskInstance(t, repoID, legacy.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = legacy
	manager.mu.Unlock()

	require.NoError(t, legacy.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, legacy.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, legacy))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, "errored: successor failed", got.LastRunStatus)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredTaskRuntimePreservesLaterWatcherSupervisionStatus(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0007", repoPath)
	tsk = addStatusTestTask(t, tsk)
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "supervision-wins", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1, TaskRunRevision: 0,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, runAt, "started")
	require.NoError(t, err)
	_, err = task.UpdateTaskStatus(tsk.ID, nil, "errored: watcher exited")
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, "errored: watcher exited", got.LastRunStatus)
	assert.Equal(t, inst.ID, got.LastRunSessionID)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredTaskRuntimeDoesNotClaimRecreatedTaskID(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	oldTask := enabledCronTask("dead0010", repoPath)
	oldTask = addStatusTestTask(t, oldTask)
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	oldSession, err := session.NewInstance(session.InstanceOptions{
		Title: "prior-incarnation", Path: repoPath, Program: "claude", TaskID: oldTask.ID,
		TaskGenerationID: oldTask.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1, TaskRunRevision: 0,
	})
	require.NoError(t, err)
	oldSession.SetStartedForTest(true)
	oldSession.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repoID, oldSession.Title)
	seedDiskInstance(t, repoID, oldSession.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = oldSession
	manager.mu.Unlock()

	require.NoError(t, task.RemoveTask(oldTask.ID, task.ProjectExpectation{}))
	replacement := enabledCronTask(oldTask.ID, repoPath)
	replacement.Name = "replacement"
	replacement = addStatusTestTask(t, replacement)
	require.NotEqual(t, oldTask.GenerationID, replacement.GenerationID)

	require.NoError(t, oldSession.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, oldSession.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, oldSession))

	got, err := task.GetTask(replacement.ID)
	require.NoError(t, err)
	assert.Empty(t, got.LastRunStatus,
		"a surviving session from the removed task must not write an outcome onto its replacement")
	assert.Empty(t, got.LastRunSessionID)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredLegacyTaskRuntimeDoesNotClaimPersistedUnloadedSuccessor(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0008", repoPath)
	createdAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	successorAt := createdAt.Add(time.Minute)
	tsk = addStatusTestTask(t, tsk)
	_, err := task.UpdateTaskStatus(tsk.ID, &successorAt, task.RunStatusStarted)
	require.NoError(t, err)

	legacy, err := session.NewInstance(session.InstanceOptions{
		Title: "legacy-materialized", Path: repoPath, Program: "claude",
		TaskID: tsk.ID, TaskGenerationID: tsk.GenerationID, CreatedAt: createdAt,
	})
	require.NoError(t, err)
	legacy.SetStartedForTest(true)
	legacy.SetStatusForTest(session.Running)
	ghost := session.InstanceData{
		ID: "unloaded-successor", Title: "unloaded-successor", Path: repoPath,
		TaskID: tsk.ID, TaskGenerationID: tsk.GenerationID,
		TaskRunActive: true, CreatedAt: successorAt,
		Status: session.Running, Liveness: session.LiveRunning,
	}
	raw, err := json.Marshal([]session.InstanceData{legacy.ToInstanceData(), ghost})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, raw))
	key := daemonInstanceKey(repoID, legacy.Title)
	manager.mu.Lock()
	manager.instances[key] = legacy // ghost deliberately failed materialization
	manager.mu.Unlock()

	require.NoError(t, legacy.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, legacy.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, legacy))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.RunStatusStarted, got.LastRunStatus)
	assert.Empty(t, got.LastRunSessionID)
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func TestRestoredTaskRuntimeDoesNotClaimTargetSessionDelivery(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0009", repoPath)
	tsk.TargetSession = "shared-target"
	tsk = addStatusTestTask(t, tsk)
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "old-per-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1, TaskRunRevision: 0,
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	targetAt := runAt.Add(time.Minute)
	// Older binaries recorded a missing shared target's successful auto-create as
	// "started". The task's declared target is the provenance that proves this is
	// not the old per-run session's row.
	_, err = task.UpdateTaskStatus(tsk.ID, &targetAt, task.RunStatusStarted)
	require.NoError(t, err)
	key := daemonInstanceKey(repoID, inst.Title)
	seedDiskInstance(t, repoID, inst.Title, repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.NoError(t, inst.Transition(session.MarkRestoring()))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.RunStatusStarted, got.LastRunStatus)
	require.NotNil(t, got.LastRunAt)
	assert.True(t, got.LastRunAt.Equal(targetAt))
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}

func addStatusTestTask(t *testing.T, tsk task.Task) task.Task {
	t.Helper()
	created, err := task.AddTaskChecked(tsk, task.ActorUnknown, nil)
	require.NoError(t, err)
	return created
}
