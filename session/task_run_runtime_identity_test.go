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
		require.NoError(t, inst.Transition(ConfirmLive()))
		require.False(t, inst.TaskRunActive(),
			"a replacement runtime that never received the run prompt must not own the active run")

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"the replacement runtime's first idle observation must not complete the interrupted run")
	})

	t.Run("prompted runtime still completes on its idle edge", func(t *testing.T) {
		inst := &Instance{
			TaskID:        "task-id",
			liveness:      LiveRunning,
			taskRunActive: true,
		}

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"the runtime that received the prompt still completes its run when it goes idle")
	})
}
