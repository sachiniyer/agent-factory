package session

import (
	"bytes"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
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
// The durable completion boundary is the agent's own in-turn chrome observed
// after the attempt — the elapsed-timer status row must tick, which a booting
// pane cannot produce — or, for agents with no in-turn signature, post-attempt
// churn followed by quiet longer than the completion grace. Plain post-attempt
// churn is deliberately NOT the boundary: the same async pipeline that hides
// devin's boot renders its prompt echo, ACP init, and skill discovery as
// ordinary Updated captures seconds after Enter.

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
	require.True(t, inst.taskRunTurnGateHeld,
		"the edge is HELD, not silently consumed — the next post-boundary idle observation retires it")
	require.False(t, inst.taskRunIdleEdgeHeld,
		"the mission-hold marker stays clear — task_run_idle_edge_held keeps its pre-#5219 meaning for rollback")
	require.Equal(t, LiveReady, inst.GetLiveness(),
		"the observation still applies on the liveness axis — only the run-end effect is withheld")
}

// TestTaskRunBootChurnDoesNotReleaseTheHold is the review shape: send at T,
// the prompt's own echo plus ACP-init/skill-discovery output renders at T+6s —
// ordinary churn a still-booting pane produces on its own — and the next quiet
// tick follows. That churn is not turn evidence; the edge must stay held.
func TestTaskRunBootChurnDoesNotReleaseTheHold(t *testing.T) {
	inst := taskRunSession(t)

	// Backdate the attempt so the +6s churn lands in the past — evidence
	// stamps are wall-clock and a future timestamp would read as activity that
	// postdates obligations filed later (the lifecycle adoption guard).
	attemptedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// Post-send pane churn from echo/boot, only seconds after the send —
	// exactly the timing that used to release the hold.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(6*time.Second), epoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"echo/ACP-init churn seconds after the send is not the agent taking the turn — the edge must stay held")
	require.True(t, inst.taskRunTurnGateHeld)

	// And it STAYS held on quiet Ready→Ready ticks while the boot churn is
	// still fresh — the held flag does not consume itself on the next idle.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive())
}

// The run ends on the first idle edge after the agent's own in-turn chrome is
// observed — the one signal a still-booting pane cannot produce.
func TestTaskRunIdleEdgeAfterTurnChromeEndsRun(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// The poll's chrome watcher saw the elapsed-timer row tick — the same
	// proof the send path's post-submit watch requires.
	require.True(t, inst.RecordTaskRunTurn(attemptedAt.Add(4*time.Second)))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"once in-turn chrome was observed after the attempt, the idle edge ends the run")
}

// The fallback arm: agents with no in-turn signature (and turns too fast for
// two captures to catch the timer) complete on post-attempt churn followed by
// sustained quiet past the completion grace. A boot that keeps burping output
// resets the window — only silence after the last observed output qualifies.
func TestTaskRunSustainedQuietAfterChurnEndsRun(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now().Add(-2 * taskRunCompletionQuietGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// Churn post-attempt, but long enough ago that the pane has sat silent
	// past the grace — a completed unit of work gone quiet, not a boot burst.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(taskRunCompletionQuietGrace), epoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"post-attempt churn followed by a full grace of quiet may stand in for the chrome")
}

// A run completed by the quiet-fallback arm — no in-turn chrome ever seen —
// must say so distinctly in the log, naming the agent and the attempt's age
// (#5219). A chrome-released run must not carry the line.
func TestTaskRunQuietFallbackReleaseLogsDistinctly(t *testing.T) {
	var buf bytes.Buffer
	old := log.InfoLog.Writer()
	log.InfoLog.SetOutput(&buf)
	defer log.InfoLog.SetOutput(old)

	inst := taskRunSession(t)
	attemptedAt := time.Now().Add(-2 * taskRunCompletionQuietGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))

	// Held while no evidence exists: the arm has not run, nothing logged.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive())
	require.NotContains(t, buf.String(), "quiet fallback")

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(attemptedAt.Add(taskRunCompletionQuietGrace), epoch))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive())
	out := buf.String()
	require.Contains(t, out, "quiet fallback")
	require.Contains(t, out, "agent=claude")
	require.Contains(t, out, "elapsed_since_attempt=")

	// In-turn chrome releases through the strong arm instead — no line.
	chrome := taskRunSession(t)
	attemptedAt = time.Now().Add(-10 * time.Second)
	require.True(t, chrome.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.True(t, chrome.RecordTaskRunTurn(attemptedAt.Add(4*time.Second)))
	buf.Reset()
	require.NoError(t, chrome.Transition(ObserveLiveness(LiveReady)))
	require.False(t, chrome.TaskRunActive())
	require.NotContains(t, buf.String(), "quiet fallback")
}

// A held edge is released while the session stays Ready: the paused poll path
// (observeTaskRunWhilePaused) folds evidence in WITHOUT a liveness move, so the
// turn boundary can arrive on a Ready→Ready observation. The mission hold
// already covers that shape through the flag; the turn gate must too.
func TestTaskRunHeldEdgeReleasedByReadyToReadyTurn(t *testing.T) {
	inst := taskRunSession(t)

	attemptedAt := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(), "precondition: the delivery-window edge was held")

	// The chrome observation lands while the row is already Ready — epoch-
	// fenced, exactly as the poll applies it.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordTaskRunTurnAtEpoch(attemptedAt.Add(3*time.Second), epoch))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"the held edge completes once the turn boundary is proven")
	require.False(t, inst.taskRunTurnGateHeld, "the consumed hold retires with the run")
}

// Boot output and turn chrome that PREDATE the prompt are not evidence for it
// — the gate orders both strictly after the task-prompt boundary.
func TestTaskRunIdleEdgeIgnoresPreAttemptEvidence(t *testing.T) {
	inst := taskRunSession(t)

	boot := time.Now().Add(-time.Minute)
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(boot, epoch),
		"precondition: boot output recorded before the prompt send")

	attemptedAt := boot.Add(time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.True(t, inst.RecordTaskRunTurn(boot),
		"a stale chrome stamp is stored but ordered before the boundary")
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))

	require.True(t, inst.TaskRunActive(),
		"evidence that predates the attempt is the previous turn, not this prompt's")
}

// The gate binds to the run's OWN prompt boundary, not the session's latest
// send (#5221 review P1): once the turn is taken, an interactive send — even
// one affirmatively refused by the pane — must not re-arm the delivery window
// and wedge the run open waiting for churn a dead send cannot produce.
func TestTaskRunManualSendAfterTurnDoesNotRearm(t *testing.T) {
	for _, status := range []PromptDeliveryStatus{PromptSentUnverified, PromptNotDelivered} {
		inst := taskRunSession(t)

		attemptedAt := time.Now().Add(-time.Minute)
		require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
		require.True(t, inst.RecordTaskRunTurn(attemptedAt.Add(4*time.Second)))

		// A manual af sessions send while the run is still active. The
		// session-level evidence updates (the row's idle reason tracks it),
		// but the task-prompt boundary is frozen.
		manualAt := attemptedAt.Add(30 * time.Second)
		require.True(t, inst.RecordPromptAttempt(status, manualAt))

		require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
		require.False(t, inst.TaskRunActive(),
			"a manual send (status %s) must not reopen a window the turn evidence already closed", status)
	}
}

// While the window is still UNSATISFIED a later send does move the task
// boundary: it is the redelivery path — the only way out once a send proved
// the pane would not take the prompt.
func TestTaskRunResendWhileUnsatisfiedReArmsBoundary(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-2 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	// Churn and quiet that would satisfy the ORIGINAL window — then a resend
	// moves the boundary past it.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(30*time.Second), epoch))
	t1 := t0.Add(60 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the resend's window supersedes churn that predates it")

	// The redelivery's own turn evidence releases it.
	require.True(t, inst.RecordTaskRunTurn(t1.Add(2*time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive())
}

// A failed manual send while the window is still open must not overwrite the
// standing boundary (#5221 review): PromptNotDelivered proves the pane took
// nothing, so it cannot supersede an armed window — otherwise a declined poke
// would strand a quiet-fallback release the task's own churn already earned.
func TestTaskRunFailedManualSendKeepsArmedWindow(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-2 * taskRunCompletionQuietGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(taskRunCompletionQuietGrace), epoch))

	// The operator pokes the pane mid-window and the send is refused. The
	// session-level evidence records the failure (the row's idle reason
	// tracks it) but the task boundary must stay at the task's send.
	require.True(t, inst.RecordPromptAttempt(PromptNotDelivered, t0.Add(taskRunCompletionQuietGrace+10*time.Second)))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"a refused manual send does not unsatisfy the window the task's churn already earned")
}

// Chrome observed while the boundary says the prompt never landed is not
// evidence for it (#5221 review): recording it would fake a satisfied window
// and freeze the boundary so a later redelivery could never re-arm.
func TestTaskRunChromeOnFailedBoundaryDoesNotBlockRedelivery(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptNotDelivered, t0))

	// Unrelated in-turn chrome (the operator started work by hand) must not
	// record — the prompt provably never arrived.
	require.False(t, inst.RecordTaskRunTurn(t0.Add(5*time.Second)),
		"chrome cannot evidence a turn on a prompt that provably never landed")

	// So the redelivery still re-arms the boundary — not frozen by a stale stamp.
	t1 := time.Now().Add(-10 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	require.True(t, inst.RecordTaskRunTurn(t1.Add(2*time.Second)))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive())
}

// A row persisted by a release that predates the task-scoped fields restores
// an ACTIVE run with only session-level prompt evidence. That send is the
// task's prompt — adopt it as the boundary so the upgrade does not leave the
// delivery window unarmed and let the first Ready tick complete the run.
func TestTaskRunLegacyRowAdoptsSessionBoundary(t *testing.T) {
	inst := taskRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))

	data := inst.ToInstanceData()
	// Simulate the pre-#5221 record: session-level evidence present, the
	// task-scoped fields absent.
	data.TaskRunPromptAttemptAt = time.Time{}
	data.BackendType = "docker"

	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.True(t, reloaded.TaskRunActive(),
		"a legacy active row must keep its delivery window armed across the upgrade")
	require.True(t, reloaded.taskRunTurnGateHeld)
}

// The session-level evidence a legacy row adopts is the SESSION's latest
// send — possibly a manual one unrelated to the task's prompt. The adopted
// boundary stays releaseable by chrome or the arms rather than wedging on
// evidence that may have nothing to do with the task's prompt.
func TestTaskRunLegacyRowAdoptsLatestSessionSend(t *testing.T) {
	inst := taskRunSession(t)
	// Recent attempt — the upgrade window, not a drained one: an adopted
	// boundary older than the silent grace would (correctly) release at once.
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))

	data := inst.ToInstanceData()
	data.TaskRunPromptAttemptAt = time.Time{}
	data.BackendType = "docker"

	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.True(t, reloaded.TaskRunActive())
	require.True(t, reloaded.taskRunTurnGateHeld)

	// The adopted window is satisfiable: turn chrome ends the run rather than
	// wedging the concurrency slot on ambiguous manual evidence.
	require.True(t, reloaded.RecordTaskRunTurn(time.Now()))
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.False(t, reloaded.TaskRunActive())
}

// A send that affirmatively failed delivery does not arm the task boundary at
// all — the pane provably took nothing, so there is no delivery window to
// hold. The row still reads prompt-not-delivered from the session-level
// evidence; the run itself ends on the idle edge exactly as it did before the
// gate existed.
func TestTaskRunFailedSendDoesNotArmTheBoundary(t *testing.T) {
	inst := taskRunSession(t)

	require.True(t, inst.RecordPromptAttempt(PromptNotDelivered, time.Now()))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"an unarmed gate ends the run on the idle edge — master behavior on this seam")

	reason, _ := inst.IdleReasonSnapshot()
	require.Equal(t, IdleReasonPromptNotDelivered, reason,
		"the row still reads prompt-not-delivered from the session-level status")
}

// The turn record latches within its window (#5221 review P2): once an
// observation already satisfies the boundary, further ticking rows are the
// same turn continuing — accepting them would checkpoint the whole instances
// file on every poll. A re-armed boundary (redelivery while unsatisfied) takes
// its own first observation.
func TestTaskRunTurnRecordLatchesWithinWindow(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	require.True(t, inst.RecordTaskRunTurn(t0.Add(4*time.Second)))
	require.False(t, inst.RecordTaskRunTurn(t0.Add(8*time.Second)),
		"the boundary is already durable — a still-ticking row is not new evidence")
	require.False(t, inst.RecordTaskRunTurn(t0.Add(12*time.Second)))
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

// The turn boundary is durable like the held flag: a daemon restart must not
// reopen a window an observation already closed — a reloaded row whose turn
// evidence survives ends on its next idle edge instead of re-holding.
func TestTaskRunTurnEvidenceSurvivesRoundTrip(t *testing.T) {
	inst := taskRunSession(t)
	attemptedAt := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.True(t, inst.RecordTaskRunTurn(attemptedAt.Add(5*time.Second)))

	data := inst.ToInstanceData()
	require.Equal(t, attemptedAt.Add(5*time.Second).UTC(), data.TaskRunTurnObservedAt.UTC(),
		"the turn observation persists while the run is in flight")
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded.TaskRunActive(), "precondition: the run is still open after the restart")

	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.False(t, reloaded.TaskRunActive(),
		"a restart cannot reopen a delivery window the chrome observation already closed")
}

// The held flag is durable: a daemon restart inside the delivery window must
// not hand the reloaded row a spendable edge. It persists under its OWN key —
// task_run_turn_gate_held, not task_run_idle_edge_held — so a rollback to a
// release that knows only the mission hold cannot misread it as resolved and
// end the run on a Ready → Ready tick (#5221 review).
func TestTaskRunHeldEdgeSurvivesRoundTrip(t *testing.T) {
	inst := taskRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.taskRunTurnGateHeld, "precondition: the edge is held")

	reason, _ := inst.IdleReasonSnapshot()
	require.Equal(t, IdleReasonDeliveryUnconfirmed, reason,
		"a run that never produced turn evidence is reported as delivery-unknown, not as a completion")

	data := inst.ToInstanceData()
	require.True(t, data.TaskRunTurnGateHeld, "the turn-gate hold persists under its own key")
	require.False(t, data.TaskRunIdleEdgeHeld,
		"the mission-hold key stays clear: an older binary must not read this as its resolved-mission release")
	require.False(t, data.TaskRunPromptAttemptAt.IsZero(), "the task prompt boundary persists")

	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded.TaskRunActive(), "the run is still open after the restart")
	require.True(t, reloaded.taskRunTurnGateHeld, "the held edge survives the restart")

	// And it still holds on the next quiet tick — the boundary evidence
	// survived with it, so the gate is still armed.
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.True(t, reloaded.TaskRunActive())
}

// RecordTaskRunTurn is scoped to a live run: an interactive session can show
// the same chrome all day and never accumulate task-run evidence.
func TestTaskRunTurnEvidenceRequiresAnActiveRun(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{
		Title:   "interactive",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	require.False(t, inst.RecordTaskRunTurn(time.Now()),
		"a session with no task run must not accumulate turn evidence")
}

// A runtime replacement retires the run's delivery evidence with the rest of
// the retired runtime's pane facts: the replacement pane owns neither the
// send it did not make nor the chrome it did not show, so the run's
// remaining life follows the pane the same way it did before the gate
// existed. The recovery-path window this leaves is tracked in the follow-up
// issue for turn gating across replacement.
func TestTaskRunGateDoesNotSurviveRuntimeReplacement(t *testing.T) {
	inst := taskRunSession(t)
	attemptedAt := time.Now()
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, attemptedAt))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.taskRunTurnGateHeld, "precondition: the edge is held")

	inst.ClearIdleEvidence()

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"the gate is pane-relative evidence of the retired runtime — the replacement's first idle edge ends the run, as on master")
}

// A turn that fits entirely between two polls and leaves the pane byte-
// identical produces neither chrome nor churn — the quiet arm can never
// satisfy it. The silent arm bounds the wait from the attempt itself, at a
// grace long enough that a real boot cannot ride it out (#5221 review P1).
func TestTaskRunSilentTurnReleasesAfterLongGrace(t *testing.T) {
	oldGrace := taskRunCompletionSilentGrace
	taskRunCompletionSilentGrace = 2 * time.Second
	defer func() { taskRunCompletionSilentGrace = oldGrace }()

	inst := taskRunSession(t)
	// Attempt far enough back that the compressed silent grace has elapsed,
	// with zero churn recorded at any point.
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now().Add(-10*time.Second)))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"a turn invisible to the poll still completes — bounded, not wedged")
}

// The silent arm must not substitute for a short boot: an attempt inside the
// silent grace with no evidence at all still holds.
func TestTaskRunSilentArmDoesNotRideOutBoot(t *testing.T) {
	oldGrace := taskRunCompletionSilentGrace
	taskRunCompletionSilentGrace = 5 * time.Minute
	defer func() { taskRunCompletionSilentGrace = oldGrace }()

	inst := taskRunSession(t)
	// Recent attempt, no churn — the 30s quiet arm can't fire either.
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"no evidence at all is still not a turn — the run stays open inside the grace")
}

// A machinery resend of a satisfied window does not re-arm it (#5221 scope):
// the boundary froze when the turn was taken, and the run completes on the
// task's proven evidence — the same ending the run had before the gate
// distinguished send origins.
func TestTaskRunMachineryResendDoesNotRearmSatisfiedWindow(t *testing.T) {
	inst := taskRunSession(t)
	t0 := time.Now().Add(-time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	require.True(t, inst.RecordTaskRunTurn(t0.Add(5*time.Second)))

	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now()))
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"a satisfied window stays closed to every later send — machinery and manual alike")
}
