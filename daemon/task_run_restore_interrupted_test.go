package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoredTaskRuntimeIsRecordedInterruptedAndSkipsOnComplete(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := enabledCronTask("dead0001", repoPath)
	tsk.OnComplete = task.OnCompleteArchive
	require.NoError(t, task.AddTask(tsk))

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "interrupted-run", Path: repoPath, Program: "claude", TaskID: tsk.ID,
	})
	require.NoError(t, err)
	deliveredAt := inst.CreatedAt
	_, err = task.UpdateTaskStatus(tsk.ID, &deliveredAt, "started")
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
	require.NoError(t, task.AddTask(tsk))

	olderRunAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "older-run", Path: repoPath, Program: "claude", TaskID: tsk.ID, CreatedAt: olderRunAt,
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
	_, err = task.UpdateTaskStatus(tsk.ID, &newerRunAt, "started")
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
	assert.Contains(t, logs.warnings.String(), "the task row does not identify this run")
}
