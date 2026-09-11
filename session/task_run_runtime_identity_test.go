package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskRunEndsOnlyForRuntimeThatReceivedPrompt(t *testing.T) {
	t.Run("restored runtime is interrupted before its idle edge", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveRunning,
			taskRunActive: true,
		}

		require.NoError(t, inst.Transition(ObserveLiveness(LiveLost)))
		require.True(t, inst.TaskRunActive(), "losing the prompted runtime does not complete its run")
		require.NoError(t, inst.Transition(MarkRestoring()))
		taskID, _, interrupted := inst.InterruptTaskRunAtRestoreBoundary()
		require.True(t, interrupted)
		require.Equal(t, "task-id", taskID)
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
