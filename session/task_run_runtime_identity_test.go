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
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.False(t, inst.TaskRunActive(),
			"a replacement runtime that never received the run prompt must not own the active run")

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"the replacement runtime's first idle observation must not complete the interrupted run")
	})

	t.Run("restore confirmation is the structural fallback", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveLost,
			inFlightOp:    OpRestoring,
			taskRunActive: true,
		}

		require.NoError(t, inst.Transition(ConfirmLive()))
		require.False(t, inst.TaskRunActive(),
			"a caller without a settlement callback must not transfer the run to a restored runtime")
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
