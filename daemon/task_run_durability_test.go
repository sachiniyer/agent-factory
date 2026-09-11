package daemon

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestTaskRunAdmissionRefusesUnreadableTaskRow(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	tsk := addStatusTestTask(t, enabledCronTask("read0001", repoPath))
	_, err := task.UpdateTaskStatus(tsk.ID, nil, "errored: watcher exited")
	require.NoError(t, err)

	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	original, err := os.ReadFile(tasksPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, []byte("{"), 0600))
	_, admissionErr := manager.nextTaskRunAdmission(tsk.ID, tsk.GenerationID)
	require.NoError(t, os.WriteFile(tasksPath, original, 0600))

	require.Error(t, admissionErr,
		"a failed row read is not proof that the task generation and revision are zero")
	require.ErrorContains(t, admissionErr, "read task read0001 for run admission")
	require.Zero(t, manager.taskRunSequence,
		"a refused admission must not consume an ordering sequence")
}

func TestInterruptedTaskStatusWriteFailureIsRetried(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("retry001", repoPath))
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "retry-interruption", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, runAt, task.RunStatusStarted)
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

	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	original, err := os.ReadFile(tasksPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, []byte("{"), 0600))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))
	require.False(t, inst.TaskRunActive(), "the session settlement is one-way even when task storage is unavailable")
	require.Contains(t, logs.warnings.String(), "could not record last_run_status")
	require.Contains(t, logs.warnings.String(), "will retry")
	require.NoError(t, os.WriteFile(tasksPath, original, 0600))

	manager.FlushOwedSettlements()
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus,
		"the owed exact-run outcome must land after task storage recovers")
}

func TestInterruptedTaskStatusRetrySurvivesDaemonRestart(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("retry002", repoPath))
	runAt := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "restart-retry", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, runAt, task.RunStatusStarted)
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

	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	original, err := os.ReadFile(tasksPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, []byte("{"), 0600))
	require.NoError(t, manager.prepareRuntimeReplacement(repoID, key, inst))
	require.NoError(t, os.WriteFile(tasksPath, original, 0600))

	stored := persistedInstanceByTitle(t, repoID, inst.Title)
	reloaded, err := session.FromInstanceData(stored)
	require.NoError(t, err)
	restarted := &Manager{instances: map[string]*session.Instance{key: reloaded}}
	owed := persistLoadRuntimeReplacements(restarted.instances)
	restarted.mu.Lock()
	restarted.registerLoadRuntimeSettlementsLocked(owed)
	restarted.mu.Unlock()
	restarted.FlushOwedSettlements()

	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus,
		"the durable session record must reconstruct an interrupted-status obligation after restart")
}

func TestInterruptedTaskSessionWriteFailureKeepsReplacementFenced(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	tsk := addStatusTestTask(t, enabledCronTask("retry003", repoPath))
	runAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fenced-interruption", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, runAt, task.RunStatusStarted)
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

	diskFull := errors.New("no space left on device")
	failedWrites, _, heal := fullDiskFor(t, inst.Title, diskFull)
	require.ErrorIs(t, manager.prepareRuntimeReplacement(repoID, key, inst), diskFull)
	require.Positive(t, failedWrites(), "the witness must fail the interrupted-session checkpoint")
	require.NoError(t, inst.Transition(session.ConfirmLive()))
	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(),
		"the replacement must stay fenced while disk could resurrect the predecessor's active run")
	require.Equal(t, session.LiveLost, inst.GetLiveness())

	heal()
	manager.FlushOwedSettlements()
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.Equal(t, session.LiveRunning, inst.GetLiveness())
	require.False(t, persistedInstanceByTitle(t, repoID, inst.Title).TaskRunActive)
}

func TestRemovedTaskTerminatesInterruptedStatusRetry(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	runAt := time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "removed-task", Path: repoPath, Program: "claude", TaskID: "removed1",
		TaskGenerationID: "removed-generation", CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
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
	_, pending := inst.PendingTaskRunInterruption()
	require.False(t, pending, "a removed task can never accept this generation's outcome")
	manager.mu.Lock()
	_, retrying := manager.settleOwed[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	require.False(t, retrying, "terminal supersession must not become an infinite retry")
	require.Contains(t, logs.warnings.String(), "task was removed")
}

func TestTargetDeliveryStatusDoesNotCrossTaskGeneration(t *testing.T) {
	_, _, repoPath := newStatusTestManager(t)
	original := addStatusTestTask(t, enabledCronTask("target01", repoPath))
	require.NoError(t, task.RemoveTask(original.ID, task.ProjectExpectation{}))
	replacement := addStatusTestTask(t, enabledCronTask(original.ID, repoPath))
	require.NotEqual(t, original.GenerationID, replacement.GenerationID)
	runAt := time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC)

	require.NoError(t, recordDeliveredTaskRun(original.ID, taskDelivery{
		status: "sent",
		run: session.TaskRunIdentity{
			TaskID: original.ID, TaskGenerationID: original.GenerationID, RunAt: runAt,
		},
	}))

	got, err := task.GetTask(replacement.ID)
	require.NoError(t, err)
	require.Empty(t, got.LastRunStatus,
		"a delivery admitted by a removed task incarnation must not update its replacement")
	require.Nil(t, got.LastRunAt)
}
