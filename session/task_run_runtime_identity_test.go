package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskRunEndsOnlyForRuntimeThatReceivedPrompt(t *testing.T) {
	t.Run("restored runtime is interrupted before its idle edge", func(t *testing.T) {
		runAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveRunning,
			taskRunActive: true,
			taskRunAt:     runAt,
		}

		require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
		require.True(t, inst.TaskRunActive(), "losing the prompted runtime does not complete its run")
		require.NoError(t, inst.Transition(MarkRestoring()))
		run, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.True(t, interrupted)
		require.Equal(t, "task-id", run.TaskID)
		require.True(t, run.RunAt.Equal(runAt))
		require.False(t, inst.TaskRunActive(),
			"the settlement boundary must durably close the predecessor's run")
		require.NoError(t, inst.Transition(ConfirmRuntimeReplacementLive()))
		require.False(t, inst.TaskRunActive(),
			"a replacement runtime that never received the run prompt must not own the active run")

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"the replacement runtime's first idle observation must not complete the interrupted run")
	})

	t.Run("replacement confirmation is the structural fallback", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLost,
			inFlightOp:    OpRestoring,
			taskRunActive: true,
		}

		require.NoError(t, inst.Transition(ConfirmRuntimeReplacementLive()))
		require.False(t, inst.TaskRunActive(),
			"a caller without a settlement callback must not transfer the run to a restored runtime")
	})

	t.Run("reattached prompted runtime retains its run", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLost,
			inFlightOp:    OpRestoring,
			taskRunActive: true,
		}
		boundaryCalled := false
		require.NoError(t, inst.withLiveBoundary(func() {
			boundaryCalled = true
			inst.InterruptTaskRunAtRuntimeReplacement()
		}, func() error {
			return inst.Transition(ConfirmLive())
		}))
		require.False(t, boundaryCalled,
			"reattachment is not runtime replacement provenance")
		require.True(t, inst.TaskRunActive(),
			"the original prompted runtime still owns and may complete its run")
	})

	t.Run("prompt redelivery fence preserves the run", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLimitReached,
			inFlightOp:    OpRespawning,
			taskRunActive: true,
		}

		_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.False(t, interrupted)
		require.True(t, inst.TaskRunActive(),
			"OpRespawning promises prompt redelivery and must retain the queued run")
	})

	t.Run("pending handoff replay fence preserves the run", func(t *testing.T) {
		inst := &Instance{
			TaskID:                "task-id",
			liveness:              LiveRunning,
			inFlightOp:            OpReplacing,
			taskRunActive:         true,
			pendingHandoffMission: "continue the inherited work",
			handoffDeliveryStatus: PromptNotDelivered,
		}

		_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.False(t, interrupted)
		require.True(t, inst.TaskRunActive(),
			"OpReplacing carries a durable mission that will prompt the replacement runtime")
	})

	t.Run("pending account-swap replay preserves the run", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLimitReached,
			taskRunActive: true,
			pendingAccountSwap: &AccountSwapData{
				Manual: true, From: "work", To: "personal", Mission: "continue the task",
				ReplacementPanesStarted: true, MissionDeliveryStatus: PromptNotDelivered,
			},
		}

		_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.False(t, interrupted)
		require.True(t, inst.TaskRunActive(),
			"positive transaction-scoped non-delivery promises the replacement the pending prompt")
	})

	t.Run("ordinary limit-parked load replacement preserves the queued run", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			Prompt:        "run the scheduled audit",
			liveness:      LiveLimitReached,
			taskRunActive: true,
		}

		_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.False(t, interrupted)
		require.True(t, inst.TaskRunActive(),
			"the limit scheduler will deliver the stored prompt to the replacement runtime")
	})

	t.Run("ambiguous account-swap delivery interrupts the run", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLimitReached,
			taskRunActive: true,
			pendingAccountSwap: &AccountSwapData{
				Manual: true, From: "work", To: "personal", Mission: "continue the task",
				ReplacementPanesStarted: true, MissionDeliveryStatus: PromptCouldNotConfirm,
			},
		}

		_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
		require.True(t, interrupted)
		require.False(t, inst.TaskRunActive(),
			"an ambiguous mission may already have run and cannot authorize automatic replay")
	})

	t.Run("prompted runtime still completes on its idle edge", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveReady,
			taskRunActive: true,
		}

		require.NoError(t, inst.Transition(BeginCreate()))
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.True(t, inst.TaskRunActive(),
			"confirming the original runtime must retain the run for prompt delivery")
		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"the runtime that received the prompt still completes its run when it goes idle")
	})
}

func TestTaskRunIdentityPersistsWithoutInventingOneForLegacyRecords(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 9, 0, 0, 123, time.UTC)
	inst, err := NewInstance(InstanceOptions{
		Title: "identified", Path: t.TempDir(), Program: "claude", TaskID: "task-id",
		TaskGenerationID: "task-generation", CreatedAt: runAt, TaskRunAt: runAt,
		TaskRunSequence: 17, TaskRunRevision: 23,
	})
	require.NoError(t, err)
	stored := inst.ToInstanceData().ForStorage()
	require.True(t, stored.TaskRunAt.Equal(runAt))
	require.Equal(t, uint64(17), stored.TaskRunSequence)
	require.Equal(t, "task-generation", stored.TaskGenerationID)
	require.Equal(t, uint64(23), stored.TaskRunRevision)
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	require.True(t, reloaded.TaskRunAt.Equal(runAt))
	require.Equal(t, uint64(17), reloaded.TaskRunSequence)
	require.Equal(t, "task-generation", reloaded.TaskGenerationID)
	require.Equal(t, uint64(23), reloaded.TaskRunRevision)

	legacy := stored
	legacy.TaskRunAt = time.Time{}
	legacy.TaskRunSequence = 0
	legacy.TaskGenerationID = ""
	legacy.TaskRunRevision = 0
	raw, err = json.Marshal(legacy)
	require.NoError(t, err)
	reloaded = InstanceData{}
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	require.True(t, reloaded.TaskRunAt.IsZero(),
		"a pre-field record must remain distinguishable for compatibility attribution")
	require.Zero(t, reloaded.TaskRunSequence)
	require.Empty(t, reloaded.TaskGenerationID)
	require.Zero(t, reloaded.TaskRunRevision)
}

func TestPendingTaskRunInterruptionSurvivesStorageRoundTrip(t *testing.T) {
	runAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	inst := &Instance{
		ID: "session-id", TaskID: "task-id", taskGenerationID: "generation-id",
		Title: "interrupted", liveness: LiveLost, inFlightOp: OpRestoring,
		taskRunActive: true, taskRunAt: runAt, taskRunSequence: 7, taskRunRevision: 9,
		CreatedAt: runAt,
	}
	run, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
	require.True(t, interrupted)

	stored := inst.ToInstanceData().ForStorage()
	stored.BackendType = "docker"
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.True(t, decoded.TaskRunInterruptionPending)
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	pending, ok := restored.PendingTaskRunInterruption()
	require.True(t, ok)
	require.Equal(t, run, pending)

	wrong := pending
	wrong.Sequence++
	require.False(t, restored.ClearPendingTaskRunInterruption(wrong),
		"a stale retry must not clear another run's durable outcome")
	require.True(t, restored.ClearPendingTaskRunInterruption(pending))
	require.False(t, restored.ToInstanceData().TaskRunInterruptionPending)
}

func TestRuntimeReplacementSettlementHoldOwnsRestoreFenceUntilRelease(t *testing.T) {
	inst := &Instance{
		ID: "session-id", TaskID: "task-id", taskGenerationID: "generation-id",
		Title: "held-replacement", liveness: LiveLost, inFlightOp: OpRestoring,
		taskRunActive: true,
	}
	_, interrupted := inst.InterruptTaskRunAtRuntimeReplacement()
	require.True(t, interrupted)
	require.True(t, inst.HoldRuntimeReplacementUntilSettlement())

	require.NoError(t, inst.Transition(ConfirmLive()))
	require.Equal(t, OpRestoring, inst.GetInFlightOp())
	require.Equal(t, LiveLost, inst.GetLiveness())
	require.False(t, inst.EndRecoverFence(),
		"the restore owner's deferred release must not bypass the settlement hold")

	released, err := inst.ReleaseRuntimeReplacementAfterSettlement()
	require.NoError(t, err)
	require.True(t, released)
	require.Equal(t, OpNone, inst.GetInFlightOp())
	require.Equal(t, LiveRunning, inst.GetLiveness())
}
