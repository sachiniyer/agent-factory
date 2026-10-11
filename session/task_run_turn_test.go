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

// observeContinuously stamps a pane observation streak covering [from, now]
// at the 1s live-poll cadence — the shape the daemon leaves when it has
// watched the pane the whole window. A single backdated stamp is something
// else under the streak-expiry contract: the pane was observed once and then
// went unobserved, which the release arms must not count.
func observeContinuously(inst *Instance, from time.Time) {
	for at := from; !at.After(time.Now()); at = at.Add(5 * time.Second) {
		inst.NotePaneCaptureObserved(at, time.Second)
	}
}

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

	// The poll has been capturing since the send — the observation streak
	// covers the whole churn→quiet window before any release evaluates.
	observeContinuously(inst, attemptedAt)

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
	observeContinuously(inst, attemptedAt)
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

	observeContinuously(inst, t0)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"a refused manual send does not unsatisfy the window the task's churn already earned")
}

// A redelivery landing while the pane is still churning binds a CONTENDED
// boundary (#5221 review): the in-flight turn's continued output is not
// attributable to the new prompt, so it must not satisfy the churn arm when
// the old work goes quiet. The still edge — the first confirmed-unchanged
// capture after the send — is what re-arms churn attribution; without it
// the bounded silent arm is the only release, measured from the stillness
// that followed the activity.
func TestTaskRunRedeliveryMidTurnDoesNotReleaseOnOldChurn(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	// An in-flight turn churns right up to the redelivery.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(20 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	require.True(t, inst.taskRunStillBaseAt.IsZero(),
		"a boundary bound over a churning pane owes an observed still edge")
	_, epoch = inst.InFlightOpAndEpoch() // the rebind advanced the epoch

	// The OLD turn keeps churning after the redelivery, then goes quiet for
	// longer than the quiet grace — the shape that used to release here.
	require.True(t, inst.RecordPaneChurnAtEpoch(t1.Add(10*time.Second), epoch))
	observeContinuously(inst, t0)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the old turn's churn→quiet must not satisfy the redelivered prompt's window")

	// One unchanged capture only OPENS the still edge — a static thinking
	// frame is a sample, not proof the old turn ended (#5221 review).
	_, epoch = inst.InFlightOpAndEpoch() // the held transition advanced the epoch
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second), epoch))
	require.True(t, inst.taskRunStillBaseAt.IsZero(),
		"a single still frame must not latch the edge")
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the old turn may resume after one static frame — no release yet")
	// Stillness sustained past the grace latches the edge at its START, and
	// the bounded arm spends the run on the silence that followed ALL
	// activity.
	_, epoch = inst.InFlightOpAndEpoch() // the held transition advanced the epoch
	require.True(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second+taskRunStillGrace), epoch))
	require.Equal(t, t1.Add(15*time.Second), inst.taskRunStillBaseAt,
		"the edge latches where the sustained stillness began")
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"once every arm's evidence is attributable, the held edge may release")
}

// A still run interrupted by churn before the grace never latches: the
// unchanged frames were a pause in the old turn, not its end — the edge
// re-derives from the next uninterrupted quiet stretch (#5221 review).
func TestTaskRunStillEdgeRetiresCandidateOnChurn(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(20 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	_, epoch = inst.InFlightOpAndEpoch()

	// Still for half the grace, then the old turn resumes: the candidate
	// dies with the run it sampled.
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(10*time.Second), epoch))
	require.True(t, inst.RecordPaneChurnAtEpoch(t1.Add(10*time.Second+taskRunStillGrace/2), epoch))
	require.True(t, inst.taskRunStillCandidateAt.IsZero(),
		"churn inside the grace retires the open candidate")

	// The next still run starts its own window — it cannot borrow the
	// interrupted one's elapsed time.
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(20*time.Second), epoch))
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(20*time.Second+taskRunStillGrace-time.Second), epoch),
		"the second run does not latch early on borrowed time")
	require.True(t, inst.NotePaneStillAtEpoch(t1.Add(20*time.Second+taskRunStillGrace), epoch))
	require.Equal(t, t1.Add(20*time.Second), inst.taskRunStillBaseAt,
		"the edge latches at the uninterrupted run's start")
}

// A blind window inside the still run — a failed capture, or a stall that
// rebases the observation streak — breaks the continuity the candidate
// claims: the pane may have churned unseen inside it, so the still run owes
// a fresh window from the next confirmed capture (#5221 review).
func TestTaskRunStillEdgeRetiresCandidateOnBlindWindow(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(20 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	_, epoch = inst.InFlightOpAndEpoch()

	// A failed capture inside the candidate's window ends the run — the
	// blind interval could hide the churn that proves it was a pause.
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(10*time.Second), epoch))
	inst.NotePaneCaptureFailed()
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(10*time.Second+taskRunStillGrace), epoch),
		"the failed capture retires the candidate; this frame re-opens it")
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(10*time.Second+taskRunStillGrace+time.Second), epoch))
	require.True(t, inst.taskRunStillBaseAt.IsZero(),
		"stillness straddling a blind window never latches")

	// The same holds for a stall past the gap horizon: the re-anchor clears
	// the candidate before this frame's still stamp re-opens it.
	inst.NotePaneCaptureObserved(t1.Add(30*time.Second), time.Second)
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(40*time.Second), epoch))
	inst.NotePaneCaptureObserved(t1.Add(40*time.Second+2*paneObservationSlack), time.Second)
	require.True(t, inst.taskRunStillCandidateAt.IsZero(),
		"a re-anchored streak clears the candidate it straddled")
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(40*time.Second+2*paneObservationSlack), epoch))
	require.Equal(t, t1.Add(40*time.Second+2*paneObservationSlack), inst.taskRunStillCandidateAt,
		"the post-hole still frame opens a fresh candidate")
}

// Churn AFTER the latched still base is the attributable evidence the quiet
// arm consumes — and it is durable state, so its first frame must force a
// write even when an earlier post-attempt churn already spent the
// prompt-edge checkpoint: a restart keeping only pre-base churn would see a
// pane that never moved post-base and demote the release to the silent arm
// (#5221 review).
func TestTaskRunPostBaseChurnForcesCheckpoint(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	_, epoch := inst.InFlightOpAndEpoch()
	// The in-flight turn is still churning when the redelivery lands.
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(20 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	_, epoch = inst.InFlightOpAndEpoch()

	// The old turn churns past the rebind — spending the prompt-edge
	// checkpoint — then stillness latches the base.
	_, promptCheckpoint := inst.RecordPaneChurnCheckpointAtEpoch(t1.Add(10*time.Second), epoch)
	require.True(t, promptCheckpoint, "first post-attempt churn checkpoints")
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second), epoch))
	require.True(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second+taskRunStillGrace), epoch))

	// The new turn's first churn sits after the base: attributable evidence,
	// and durable — a second checkpoint must carry it.
	_, baseCheckpoint := inst.RecordPaneChurnCheckpointAtEpoch(t1.Add(16*time.Second+taskRunStillGrace), epoch)
	require.True(t, baseCheckpoint,
		"the first post-base churn must persist — it is the quiet arm's evidence on restore")
	// Spinner churn after it is the same turn continuing, not a new edge.
	_, again := inst.RecordPaneChurnCheckpointAtEpoch(t1.Add(20*time.Second+taskRunStillGrace), epoch)
	require.False(t, again, "no write storm for ordinary churn")
}

// The same contended boundary releases on the quiet arm when the pane goes
// still and then churns AGAIN — output after the still edge is attributable
// to the redelivered prompt, not to whatever was running before it (#5221
// review).
func TestTaskRunRedeliveryMidTurnReleasesOnPostStillChurn(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(20 * time.Second)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	_, epoch = inst.InFlightOpAndEpoch()

	// The pre-boundary turn ends — stillness sustained past the grace
	// latches the edge at its start.
	require.False(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second), epoch))
	require.True(t, inst.NotePaneStillAtEpoch(t1.Add(15*time.Second+taskRunStillGrace), epoch))
	// The redelivered prompt's own turn churns and finishes — more than a
	// quiet grace ago, so the arm's silence is already satisfied.
	require.True(t, inst.RecordPaneChurnAtEpoch(t1.Add(16*time.Second+taskRunStillGrace), epoch))
	observeContinuously(inst, t0)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"churn that began after the observed still edge is the redelivery's own turn evidence")
}

// A redelivery onto an already-quiet pane is NOT contended: churn stale
// beyond the quiet grace proves the prior activity ended before the send,
// so the boundary seeds its stillness base at the attempt and ordinary
// post-attempt churn counts immediately (#5221 review).
func TestTaskRunRedeliveryOntoQuietPaneSeedsStillBase(t *testing.T) {
	inst := taskRunSession(t)

	t0 := time.Now().Add(-3 * taskRunCompletionSilentGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))
	// The prior turn ended a full grace before the redelivery.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(10*time.Second), epoch))
	t1 := t0.Add(10*time.Second + taskRunCompletionQuietGrace)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t1))
	require.Equal(t, t1, inst.taskRunStillBaseAt,
		"a pane already quiet at the redelivery seeds the stillness base at the send")
	_, epoch = inst.InFlightOpAndEpoch()

	require.True(t, inst.RecordPaneChurnAtEpoch(t1.Add(5*time.Second), epoch))
	observeContinuously(inst, t0)
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"an uncontended redelivery keeps the ordinary churn→quiet contract")
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
	// with zero churn recorded at any point. The poll has been watching since
	// the send — the window is elapsed OBSERVED silence, not wall time.
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, time.Now().Add(-10*time.Second)))
	observeContinuously(inst, time.Now().Add(-10*time.Second))

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
	inst.NotePaneCaptureObserved(time.Now(), time.Second)

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

// A daemon restart must not inherit elapsed quiet it never observed (#5221
// review): churn after the first post-prompt edge is deliberately not
// re-checkpointed, so a restarted row can carry a lastPaneChurnAt far older
// than the pane's true last output. The quiet arm's silence clock measures
// from the later of the durable stamp and the first capture this process
// actually observed — startup materialization is not silence either.
func TestTaskRunQuietArmDoesNotInheritStaleChurnAcrossRestart(t *testing.T) {
	inst := taskRunSession(t)
	t0 := time.Now().Add(-2 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	// Checkpointed churn is minutes old by the time the daemon restarts —
	// the later, uncheckpointed churn is gone, but the durable stamp pretends
	// the pane has been silent the whole time.
	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(30*time.Second), epoch))

	data := inst.ToInstanceData()
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)

	// Before any capture the process has observed nothing at all — however
	// long materialization took, none of it counts as quiet.
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.True(t, reloaded.TaskRunActive(),
		"an unobserved pane is not a quiet pane — the clock has no floor until the first capture")

	// The first post-restart capture lands (request-start stamp). Only now
	// does the quiet clock have a floor — one unchanged frame cannot satisfy
	// the whole 30s window.
	reloaded.NotePaneCaptureObserved(time.Now(), time.Second)
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveRunning)))
	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.True(t, reloaded.TaskRunActive(),
		"silence is measured from the first observed capture — the unobserved gap is not quiet")
	require.True(t, reloaded.taskRunTurnGateHeld)
}

// A failed first send must not come back from a restart as an armed
// boundary (#5221 review): while the daemon is up, recordPromptAttemptLocked
// refuses to bind a PromptNotDelivered attempt, and the restore-side
// adoption of the session-level send must apply the same exclusion — the
// first Ready observation ends the run exactly as it would have without the
// restart, not five minutes later off the silent fallback.
func TestTaskRunFailedSendDoesNotRestoreAsArmedBoundary(t *testing.T) {
	inst := taskRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptNotDelivered, time.Now().Add(-time.Minute)))

	data := inst.ToInstanceData()
	data.BackendType = "docker"
	reloaded, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)

	require.NoError(t, reloaded.Transition(ObserveLiveness(LiveReady)))
	require.False(t, reloaded.TaskRunActive(),
		"a proven-failed send is no boundary — restore matches the un-restarted run's first-Ready end")
}

// A hole in the capture streak — failed probes or a stalled status loop —
// hides any churn the pane produced inside it, so the quiet arm's clock
// restarts at the capture that ends the hole rather than counting
// unobserved time as silence (#5221 review).
func TestTaskRunQuietArmRebasesAfterObservationGap(t *testing.T) {
	inst := taskRunSession(t)
	t0 := time.Now().Add(-2 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(30*time.Second), epoch))

	// The poll watched from the send through t0+40s, then went dark — failed
	// snapshots or a stalled loop — long past the stall horizon.
	inst.NotePaneCaptureObserved(t0.Add(40*time.Second), time.Second)
	inst.NotePaneCaptureObserved(time.Now(), time.Second)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the hidden interval is not silence — the quiet clock restarts at the capture that ends the gap")
	require.True(t, inst.taskRunTurnGateHeld)
}

// Captures arriving on their declared schedule are continuous observation,
// however far apart the schedule spaces them (#5221 review): a TUI-attached
// task run is probed only once per 30s backstop, so a horizon sized for the
// 1s live tick would rebase the streak on every paused probe and the quiet
// arm could never satisfy. The streak's gap slack follows the caller's own
// cadence — a paused probe 30s after the last is scheduled, not a hole.
func TestTaskRunQuietArmPreservesScheduledBackstopCadence(t *testing.T) {
	inst := taskRunSession(t)
	backstop := 30 * time.Second
	t0 := time.Now().Add(-5 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(time.Minute), epoch))

	// Backstop probes at their 30s cadence — every gap equals the declared
	// schedule, so the streak runs unbroken from the first probe.
	for _, at := range []time.Time{
		time.Now().Add(-2 * backstop),
		time.Now().Add(-backstop),
		time.Now(),
	} {
		inst.NotePaneCaptureObserved(at, backstop)
	}

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.False(t, inst.TaskRunActive(),
		"scheduled backstop probes are continuous observation — the quiet arm keeps its progress")
}

// The streak also EXPIRES: a satisfied window must not be inherited by a run
// of failed captures. Local capture-pane failures surface as answered
// observations with no frame, so the last confirmed stamp ages out under its
// own gap slack — evaluated past it, the arms hold rather than releasing on
// silence nobody watched (#5221 review).
func TestTaskRunArmsHoldAfterObservationGoesStale(t *testing.T) {
	inst := taskRunSession(t)
	t0 := time.Now().Add(-2 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(30*time.Second), epoch))

	// One confirmed capture a minute back, then nothing that counts — failed
	// probes leave no stamp. Churn-plus-grace is long satisfied on the
	// calendar, but the observation is stale.
	inst.NotePaneCaptureObserved(t0.Add(time.Minute), time.Second)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"a lapsed streak is no observation — the arms hold until the pane is captured again")
	require.True(t, inst.taskRunTurnGateHeld)
}

// The silent arm answers the same question the quiet arm does — "did the
// daemon watch the pane sit still" — so a hole in the streak rebases its
// clock exactly the same way (#5221 review). A >grace outage after the first
// capture used to leave the arm satisfied on the first frame back: elapsed
// wall time read as silence. It must start the observed window over.
func TestTaskRunSilentArmRebasesAfterObservationGap(t *testing.T) {
	oldGrace := taskRunCompletionSilentGrace
	taskRunCompletionSilentGrace = 2 * time.Second
	defer func() { taskRunCompletionSilentGrace = oldGrace }()

	inst := taskRunSession(t)
	t0 := time.Now().Add(-10 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	// One post-attempt capture, then the loop went dark past the stall
	// horizon — a hole the pane could have churned inside.
	inst.NotePaneCaptureObserved(t0, time.Second)
	inst.NotePaneCaptureObserved(time.Now(), time.Second)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the first capture after a hole starts the observed window over — the outage was not silence")
	require.True(t, inst.taskRunTurnGateHeld)
}

// A FAILED capture inside the gap horizon is a blind window, not a quiet
// frame: success–fail–success spaced inside the paused path's 90s slack would
// otherwise leave the old streak intact and let the blind window
// retroactively count as silence (#5221 review). The next success re-anchors
// observed silence from its own instant.
func TestTaskRunFailedCaptureBreaksObservationStreak(t *testing.T) {
	inst := taskRunSession(t)
	t0 := time.Now().Add(-5 * time.Minute)
	require.True(t, inst.RecordPromptAttempt(PromptSentUnverified, t0))

	_, epoch := inst.InFlightOpAndEpoch()
	require.True(t, inst.RecordPaneChurnAtEpoch(t0.Add(time.Minute), epoch))

	backstop := 30 * time.Second
	base := time.Now().Add(-50 * time.Second)
	inst.NotePaneCaptureObserved(base, backstop)
	inst.NotePaneCaptureFailed()
	// The straddling success lands 30s after the anchor — under the 90s gap
	// horizon, so an unbroken streak would still satisfy churn+30s quiet.
	inst.NotePaneCaptureObserved(base.Add(backstop), backstop)

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	require.True(t, inst.TaskRunActive(),
		"the frame after a failed capture re-anchors the streak — only 20s of observed silence, not 50s")
	require.True(t, inst.taskRunTurnGateHeld)
}
