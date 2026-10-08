package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The idle edge is not proof a task run finished until the agent has
// demonstrably taken the turn on the delivered prompt (#5219).
//
// A task-spawned session's run used to end on the FIRST transition into
// LiveReady — the row was born Ready, confirmed Running the moment the task
// prompt send returned, and any quiet poll after that resolved a finished run.
// For an agent whose backend keeps the pane still through boot and prompt
// acceptance (devin's ACP session init takes seconds and no IsWorkingContent
// matcher covers it), that first quiet tick lands while the agent is still
// STARTING, so on_complete archived live runs ~10s after spawn.
//
// The durable completion boundary is post-attempt pane churn: the task prompt
// send records its attempt timestamp and seeds the status monitor's
// comparison baseline with the post-Enter boundary frame, so a later
// Observation.Updated is the agent's own reaction to the prompt — never the
// send's own echo. A run whose prompt was attempted but produced no churn has
// not been demonstrably picked up; its idle edge is HELD, and the run ends
// on the first idle observation after the reaction arrives — exactly the
// shape the owed-mission hold already uses (#4429).

// taskRunSession returns a task-spawned session with its run in flight,
// running under a fake backend, at LiveRunning — the state a cron session is
// in the moment the task prompt send returns.
func taskRunSession(t *testing.T) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{
		Title:   "task-run",
		Path:    t.TempDir(),
		Program: "claude",
		TaskID:  "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(Running)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")
	return inst
}

// TestTaskRunIdleEdgeHeldDuringDeliveryWindow is the issue's report at the
// session axis: the prompt attempt landed (sent-unverified, as an ACP submit
// records it), the pane has not moved since, and the first quiet poll
// resolves the session idle. The run must not end — the agent has not
// demonstrably taken the turn.
func TestTaskRunIdleEdgeHeldDuringDeliveryWindow(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now()
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// The early idle sample: the poll saw the pane unchanged and settled
	// LiveReady while the agent is still booting its ACP session.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

	require.True(t, inst.TaskRunActive(),
		"an idle edge inside the prompt-delivery window must not end the run — the agent has not demonstrably taken the turn")
	require.True(t, inst.taskRunIdleEdgeHeld,
		"the edge is HELD, not silently consumed — the next post-reaction idle observation retires it")
	require.Equal(t, LiveReady, inst.GetLiveness(),
		"the observation still applies on the liveness axis — only the run-end effect is withheld")
}

// The run ends on the first idle edge AFTER the agent demonstrably reacted:
// churn strictly later than the attempt is the turn evidence.
func TestTaskRunIdleEdgeAfterObservedTurnEndsRun(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now()
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// The agent picked the prompt up — a snapshot updated strictly after the
	// attempt's delivery baseline.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), epoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"once the agent has demonstrably reacted, the idle edge ends the run as before")
}

// A held edge is released while the session stays Ready: the paused poll path
// (observeTaskRunWhilePaused) folds churn into lastPaneChurnAt WITHOUT a
// liveness move, so the reaction can arrive on a Ready→Ready observation. The
// mission hold already covers that shape through the flag; the turn gate must
// too.
func TestTaskRunHeldEdgeReleasedByReadyToReadyChurn(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now()
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(), "precondition: the delivery-window edge was held")

	// The agent's reaction lands while the row is already Ready.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), epoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"the held edge completes once post-delivery churn proves the agent took the turn")
	require.False(t, inst.taskRunIdleEdgeHeld, "the consumed hold retires with the run")
}

// Boot output that predates the prompt is not turn evidence — only churn
// strictly after the attempt counts.
func TestTaskRunIdleEdgeIgnoresPreAttemptChurn(t *testing.T) {
	inst := taskRunSession(t)

	boot := time.Now()
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(boot, epoch),
		"precondition: boot output recorded before the prompt send")

	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, boot.Add(time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

	require.True(t, inst.TaskRunActive(),
		"churn that predates the attempt is boot noise, not the agent taking the turn")
}

// A run whose prompt was never attempted keeps the old behavior — the
// delivery-window gate keys on the attempt marker, so sends that predate
// this mechanism (or sessions restored without the evidence) are unchanged.
func TestTaskRunIdleEdgeWithoutAttemptStillEndsRun(t *testing.T) {
	inst := taskRunSession(t)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"no recorded prompt attempt means no delivery window to wait out")
}

// The held flag is durable: a daemon restart inside the delivery window must
// not hand the reloaded row a spendable edge.
func TestTaskRunHeldEdgeSurvivesRoundTrip(t *testing.T) {
	inst := taskRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.taskRunIdleEdgeHeld, "precondition: the edge is held")

	reason, _ := inst.IdleReasonSnapshot()
	require.Equal(t, IdleReasonDeliveryUnconfirmed, reason,
		"a run that never produced turn evidence is reported as delivery-unknown, not as a completion")

	data := inst.ToInstanceData()
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded.TaskRunActive(), "the run is still open after the restart")
	require.True(t, reloaded.taskRunIdleEdgeHeld, "the held edge survives the restart")
}
