package daemon

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestPersistLoadRuntimeReplacementsCheckpointsEvidenceClear(t *testing.T) {
	_, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "restarted", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatusForTest(session.Ready)
	attemptedAt := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt)
	_, epoch := inst.InFlightOpAndEpoch()
	inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Minute), epoch)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.LoadState().SaveInstances(repoID, seeded); err != nil {
		t.Fatal(err)
	}

	inst.ClearIdleEvidence()
	inst.MarkLoadRuntimeReplacedForTest(true)
	key := daemonInstanceKey(repoID, inst.Title)
	if owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst}); len(owed) != 0 {
		t.Fatalf("successful checkpoint left %d owed settlements", len(owed))
	}
	rec := recordFor(t, repoID, inst.Title)
	if rec == nil || !rec.LastPromptAttemptAt.IsZero() || rec.LastPromptDeliveryStatus != "" || !rec.LastPaneChurnAt.IsZero() {
		t.Fatalf("persisted load-time replacement = %+v; want cleared idle evidence", rec)
	}
	if inst.ConsumeLoadRuntimeReplacement().Replaced {
		t.Fatal("load-time replacement settlement was not consumed")
	}
}

func TestPersistLoadAgentRuntimeReplacementPublishesInterruptedTaskRun(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	tsk := addStatusTestTask(t, enabledCronTask("load0001", repoPath))
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "load-interrupted", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	_, _, err = task.BeginTaskRun(tsk.ID, tsk.GenerationID, inst.ID, 1, 0, runAt, task.RunStatusStarted)
	require.NoError(t, err)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))
	inst.MarkLoadRuntimeReplacedForTest(true)
	key := daemonInstanceKey(repoID, inst.Title)
	owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst})
	require.False(t, inst.TaskRunActive())
	require.Len(t, owed, 1, "the interrupted task outcome must remain owed until the manager can publish it")

	manager.mu.Lock()
	manager.instances[key] = inst
	manager.registerLoadRuntimeSettlementsLocked(owed)
	manager.mu.Unlock()
	manager.FlushOwedSettlements()

	stored := persistedInstanceByTitle(t, repoID, inst.Title)
	require.False(t, stored.TaskRunActive,
		"the session-side interruption must be durable before task status publication")
	got, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, got.LastRunStatus)
}

func TestPersistLoadAgentRuntimeReplacementWriteFailureRefusesReplacement(t *testing.T) {
	_, repoID, repoPath := newStatusTestManager(t)
	runAt := time.Date(2026, 9, 11, 9, 15, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "load-write-failure", Path: repoPath, Program: "claude", TaskID: "load-task",
		TaskGenerationID: "load-generation", CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	inst.MarkLoadRuntimeReplacedForTest(true)

	diskFull := errors.New("no space left on device")
	failedWrites, _, _ := fullDiskFor(t, inst.Title, diskFull)
	key := daemonInstanceKey(repoID, inst.Title)
	owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst})
	require.Positive(t, failedWrites(), "the witness must fail the load-time close checkpoint")
	require.Len(t, owed, 1)
	require.True(t, owed[0].persistInstance)
	require.NotNil(t, owed[0].interruptedTaskRun)
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(),
		"without proof the replacement stopped, it must remain non-live and fenced")
	require.True(t, inst.ToInstanceData().RuntimeCleanupStateUnknown)
	require.True(t, persistedInstanceByTitle(t, repoID, inst.Title).TaskRunActive,
		"the failed write leaves the predecessor run active on disk, which is why replacement visibility is refused")
}

// TestFindSessionFallbackRegistersLoadTimeInterruptionSettlement pins the
// review finding on findSessionByStableID: when the refresh pass skips a row
// on a transient reconstruction failure but the fallback's own rebuild
// succeeds, the registered instance must not carry an unconsumed interruption
// outcome — otherwise last_run_status is never published and the durable
// marker would refuse session deletion until a daemon restart.
func TestFindSessionFallbackRegistersLoadTimeInterruptionSettlement(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	tsk := addStatusTestTask(t, enabledCronTask("load0002", repoPath))
	runAt := time.Date(2026, 9, 11, 9, 30, 0, 0, time.UTC)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fallback-interrupted", Path: repoPath, Program: "claude", TaskID: tsk.ID,
		TaskGenerationID: tsk.GenerationID, CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 1,
	})
	require.NoError(t, err)
	data := inst.ToInstanceData()
	data.TaskRunInterruptionPending = true
	seeded, err := json.Marshal([]session.InstanceData{data})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))

	// The refresh pass's reconstruction fails transiently; the fallback's own
	// retry succeeds — the exact seam the finding names.
	var calls int
	original := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(repoID string, d session.InstanceData) (*session.Instance, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient reconstruction failure")
		}
		return original(repoID, d)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = original })

	got, gotRepoID, _, err := manager.findSessionByStableID(inst.ID, inst.Title, repoID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, repoID, gotRepoID)
	key := daemonInstanceKey(repoID, inst.Title)
	manager.mu.Lock()
	tracked := manager.instances[key]
	entry, owed := manager.settleOwed[key]
	manager.mu.Unlock()
	require.Same(t, got, tracked, "the fallback instance must be the tracked one")
	if _, pending := got.PendingTaskRunInterruption(); pending {
		t.Fatal("registered instance still carries an unconsumed interruption outcome")
	}
	require.True(t, owed, "the interrupted task outcome must be queued for publication")
	require.NotNil(t, entry.interruptedTaskRun)

	manager.FlushOwedSettlements()
	stored, err := task.GetTask(tsk.ID)
	require.NoError(t, err)
	require.Equal(t, TaskStatusInterrupted, stored.LastRunStatus)
}

// The session regression drives real sibling RestoreWithResult bookkeeping;
// this test pins the daemon half of that contract without clearing agent evidence.
func TestPersistLoadRuntimeReplacementsCheckpointsSiblingTimestamp(t *testing.T) {
	_, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "sibling-restarted", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatusForTest(session.Ready)
	attemptedAt := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.LoadState().SaveInstances(repoID, seeded); err != nil {
		t.Fatal(err)
	}
	// Model the timestamp and settlement marker produced by a sibling respawn.
	inst.UpdatedAt = inst.UpdatedAt.Add(time.Hour)
	inst.MarkLoadRuntimeReplacedForTest(false)
	key := daemonInstanceKey(repoID, inst.Title)
	if owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst}); len(owed) != 0 {
		t.Fatalf("successful checkpoint left %d owed settlements", len(owed))
	}
	rec := recordFor(t, repoID, inst.Title)
	if rec == nil || !rec.UpdatedAt.Equal(inst.UpdatedAt) || !rec.LastPromptAttemptAt.Equal(attemptedAt) || rec.LastPromptDeliveryStatus != session.PromptDelivered {
		t.Fatalf("persisted sibling replacement = %+v; want updated timestamp and preserved agent evidence", rec)
	}
	if inst.ConsumeLoadRuntimeReplacement().Replaced {
		t.Fatal("settlement was not consumed")
	}
}
