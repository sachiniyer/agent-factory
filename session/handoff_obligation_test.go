package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// missionOwingInstance is a task-spawned session whose handoff completed but
// whose takeover brief is still unresolved: the fence settled on the incoming
// runtime (#4429), so the op axis is clear and only the obligation itself says
// the work is unfinished.
func missionOwingInstance(t *testing.T) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "owing", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(Running)
	inst.TaskID = "task-1"
	inst.taskRunActive = true
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, PromptSentUnverified))
	return inst
}

// An idle incoming pane is not a finished task run while its mission is still
// owed. The pane may be idle BECAUSE the brief never landed, and this edge is
// what hands a task session to its on_complete policy — so ending the run here
// is what would archive or kill a session whose mission still needs an
// operator. Before #4429 the replacement fence hid the row from the poll; it
// now settles on the incoming runtime, so the obligation has to carry this.
func TestIdleEdgeDoesNotEndTheRunWhileAMissionIsOwed(t *testing.T) {
	t.Run("agent handoff mission", func(t *testing.T) {
		inst := missionOwingInstance(t)

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

		require.True(t, inst.TaskRunActive(),
			"an idle pane with an unresolved mission has not finished its run")
		require.Equal(t, LiveReady, inst.GetLiveness(), "the liveness observation still lands")
	})

	t.Run("manual account swap mission", func(t *testing.T) {
		inst := missionOwingInstance(t)
		require.True(t, inst.ClearPendingHandoffMission("continue the inherited work"))
		inst.pendingAccountSwap = &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			Mission: "continue", MissionDeliveryStatus: PromptSentUnverified,
		}

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

		require.True(t, inst.TaskRunActive(),
			"the account-swap half of the same obligation holds the run open too")
	})

	// The guard is scoped to an owed delivery, not to handoffs in general: a row
	// that settled its mission ends its run on the very next idle edge, or the
	// task's on_complete policy would never run at all.
	t.Run("resolved mission ends the run", func(t *testing.T) {
		inst := missionOwingInstance(t)
		require.NoError(t, inst.ConfirmPendingHandoffDelivery("continue the inherited work"))
		require.NoError(t, inst.Transition(ObserveLiveness(LiveRunning)))

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

		require.False(t, inst.TaskRunActive(), "a settled row finishes its run on the idle edge")
	})

	t.Run("positive non-delivery keeps the run open as well", func(t *testing.T) {
		inst := missionOwingInstance(t)
		require.NoError(t, inst.RecordPendingHandoffMissionDelivery(
			"continue the inherited work", PromptNotDelivered))

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

		require.True(t, inst.TaskRunActive(),
			"a mission automatic recovery still owes is not a finished run either")
	})
}

// A second handoff must not start while the first one's mission is unresolved:
// SetPendingHandoffMission would overwrite the mission and its verdict, and
// nothing else records that obligation. The op axis cannot carry this, because
// #4429 settles the replacement fence on the incoming runtime's liveness.
func TestHandoffRefusedWhileAMissionIsOwed(t *testing.T) {
	inst := missionOwingInstance(t)

	err := inst.ValidateRuntimeAction(RuntimeActionHandoff)
	require.Error(t, err)
	require.ErrorContains(t, err, "still owes the mission from its last handoff")
	require.ErrorContains(t, err, "retry-limit", "the refusal names the verb that resolves it")
	require.Error(t, inst.ValidateHandoffRuntimeAction("gemini", ""),
		"the account-aware entry point refuses it too")

	// The limit-resume verb is how the obligation is resolved, so it must stay
	// admissible — it is the one action this fence may not block.
	inst.SetLimitReached(time.Now().Add(time.Hour))
	require.NoError(t, inst.ValidateRuntimeAction(RuntimeActionResumeLimit))

	// Once resolved, the row hands off again.
	require.True(t, inst.ClearPendingHandoffMission("continue the inherited work"))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveRunning)))
	require.NoError(t, inst.ValidateRuntimeAction(RuntimeActionHandoff))
}
