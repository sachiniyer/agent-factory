package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #6223: a run a restore ended as interrupted (#4222) is the same run when the
// restored agent resumes its own conversation and works. Its next idle edge must
// complete it, or the on_complete obligation is orphaned.
func interruptedRunning(t *testing.T) *Instance {
	t.Helper()
	inst := &Instance{Title: "task-run", liveness: LiveRunning, taskRunActive: true}
	inst.mu.Lock()
	inst.endTaskRunInterruptedLocked()
	inst.mu.Unlock()
	require.True(t, inst.TaskRunInterrupted())
	return inst
}

func TestInterruptedRunResumesOnInTurnEvidenceAndCompletesOnIdle(t *testing.T) {
	inst := interruptedRunning(t)
	_, ok := inst.TaskRunResumableSince(time.Now())
	require.True(t, ok)

	require.True(t, inst.ResumeInterruptedTaskRunAtEpoch(time.Now().Add(time.Second), inst.stateEpoch))
	assert.True(t, inst.TaskRunActive(), "the run is back in flight")
	assert.False(t, inst.TaskRunInterrupted())

	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	assert.False(t, inst.TaskRunActive(), "the first idle after the work ends the run")
	assert.False(t, inst.TaskRunInterrupted(), "so the daemon's completion predicate fires")
}

func TestInterruptedRunIsNotResumedByIdleBootOrLateWork(t *testing.T) {
	inst := interruptedRunning(t)
	// An idle tick alone (the replacement at rest) never completes the run.
	require.NoError(t, inst.Transition(ObserveLiveness(LiveReady)))
	assert.False(t, inst.TaskRunActive())
	assert.True(t, inst.TaskRunInterrupted())

	// Work long after the interruption is the operator's, not the task's.
	late := time.Now().Add(taskRunResumeWindow + time.Minute)
	assert.False(t, inst.ResumeInterruptedTaskRunAtEpoch(late, inst.stateEpoch))
	_, ok := inst.TaskRunResumableSince(late)
	assert.False(t, ok)
	// A stale epoch is dropped.
	assert.False(t, inst.ResumeInterruptedTaskRunAtEpoch(time.Now().Add(time.Second), inst.stateEpoch+1))
}

func TestInterruptionWindowSurvivesTheRecord(t *testing.T) {
	inst := interruptedRunning(t)
	data := inst.ToInstanceData()
	assert.False(t, data.TaskRunInterruptedAt.IsZero())
	assert.True(t, data.TaskRunInterrupted)
}

// #6231: the boundary is the replacement's launch, not the settle edge, so an
// agent already mid-turn when ConfirmLive settles still re-arms the run.
func TestInterruptionBoundaryIsTheLaunchNotTheSettle(t *testing.T) {
	inst := &Instance{Title: "task-run", liveness: LiveRunning, taskRunActive: true}
	inst.markAgentRuntimeReplaced(true)
	launched := time.Now().Add(-30 * time.Second)
	inst.mu.Lock()
	inst.agentReplacedAt = launched
	inst.endTaskRunInterruptedLocked()
	inst.mu.Unlock()

	boundary, ok := inst.TaskRunResumableSince(time.Now())
	require.True(t, ok)
	assert.True(t, boundary.Equal(launched), "boundary %v, want the launch %v", boundary, launched)
	// A turn that began after the launch but before the settle counts.
	require.True(t, inst.ResumeInterruptedTaskRunAtEpoch(launched.Add(10*time.Second), inst.stateEpoch))
}

// A replacement launched long before the interruption is not its boundary.
func TestStaleLaunchStampDoesNotWidenTheBoundary(t *testing.T) {
	inst := &Instance{Title: "task-run", liveness: LiveRunning, taskRunActive: true}
	inst.mu.Lock()
	inst.agentReplacedAt = time.Now().Add(-2 * taskRunResumeWindow)
	inst.endTaskRunInterruptedLocked()
	inst.mu.Unlock()
	boundary, ok := inst.TaskRunResumableSince(time.Now())
	require.True(t, ok)
	assert.WithinDuration(t, time.Now(), boundary, time.Second)
}

// #6230: every gate that decides whether to observe a run asks one predicate, so
// an interrupted-but-resumable run is observed (attached or not).
func TestTaskRunObservableCoversInFlightAndResumableRuns(t *testing.T) {
	inflight := &Instance{taskRunActive: true}
	assert.True(t, inflight.TaskRunObservable(time.Now()))
	assert.True(t, interruptedRunning(t).TaskRunObservable(time.Now()))
	assert.False(t, interruptedRunning(t).TaskRunObservable(time.Now().Add(taskRunResumeWindow+time.Minute)))
	assert.False(t, (&Instance{}).TaskRunObservable(time.Now()))
}

// The re-arm clears the session-wide delivery verdict alongside the run's own
// pane-relative boundary fields. The verdict belongs to the predecessor runtime
// the interruption retired — the replacement pane owns neither the send it did
// not make nor the verdict that send produced. Left in place, a stale
// PromptNotDelivered survives the #5219 forced checkpoint and pairs with the
// gate-gated unbound boundary to manufacture the exact "initial delivery proven
// undelivered" shape the restart twin in FromInstanceData treats as a terminal
// failure (#6223 review).
func TestResumeInterruptedTaskRunClearsPredecessorDeliveryVerdict(t *testing.T) {
	inst := &Instance{Title: "task-run", liveness: LiveRunning, taskRunActive: true}
	inst.mu.Lock()
	inst.lastPromptDeliveryStatus = PromptNotDelivered
	inst.lastPromptAttemptAt = time.Now()
	inst.endTaskRunInterruptedLocked()
	inst.mu.Unlock()
	require.True(t, inst.TaskRunInterrupted())

	require.True(t, inst.ResumeInterruptedTaskRunAtEpoch(time.Now().Add(time.Second), inst.stateEpoch))
	assert.True(t, inst.TaskRunActive(), "the run is back in flight")
	inst.mu.RLock()
	assert.Empty(t, inst.lastPromptDeliveryStatus,
		"the re-arm clears the predecessor-owned delivery verdict")
	assert.True(t, inst.lastPromptAttemptAt.IsZero(),
		"the re-arm retires the predecessor-owned attempt timestamp with its verdict")
	inst.mu.RUnlock()
}

// A three-restart reproduction of the wrongful teardown: a run whose initial
// delivery provably succeeded is mislabeled failed after an interrupt-then-resume
// cycle because the interrupt persist drops the gate-gated prompt-bound bit while
// the session-wide PromptNotDelivered verdict rides the record unconditionally,
// and the resume re-arms the run without clearing that verdict — so a further
// restart loads the "initial delivery proven undelivered" shape and fires the
// restart twin against a live run. Clearing the stale verdict on re-arm breaks
// the chain (#6223 review).
func TestResumedRunSurvivesRestartAfterInterruptWithStaleDeliveryVerdict(t *testing.T) {
	// Boot-0: the run's initial delivery binds its boundary, a runtime swap
	// clears the pane-relative boundary, and a later refused send records the
	// session-wide PromptNotDelivered verdict without failing the run because
	// the boundary is bound — the exact shape pinned by
	// TestTaskRunHandoffReplacementFailureDoesNotFailBoundRun (#5644).
	inst := taskRunSession(t)
	require.True(t, inst.RecordPromptAttempt(PromptDelivered, time.Now()),
		"the initial delivery binds the boundary")
	inst.ClearIdleEvidence()                                 // the runtime swap zeroes the pane-relative boundary
	inst.RecordPromptAttempt(PromptNotDelivered, time.Now()) // replacement send failed
	require.True(t, inst.TaskRunActive(),
		"a run whose prompt landed is not ended failed by a replacement send")
	require.False(t, inst.TaskRunFailed())

	// Restart 1: the bound bit survives the reload, so the restart twin does
	// not fire. A reattach whose claim cannot be verified marks the runtime
	// replaced, InterruptTaskRunIfRuntimeReplaced ends the run interrupted,
	// and the persist writes TaskRunActive=false, TaskRunInterrupted=true,
	// no TaskRunPromptBound (gate-gated), yet LastPromptDeliveryStatus=
	// PromptNotDelivered (unconditional).
	data1 := inst.ToInstanceData()
	require.True(t, data1.TaskRunPromptBound, "the bound bit is persisted while the gate is active")
	data1.BackendType = "docker"
	reloaded1, err := FromInstanceData(data1.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded1.TaskRunActive(),
		"the restart twin honours the persisted bound bit and does not fire")
	require.False(t, reloaded1.TaskRunFailed())

	reloaded1.MarkAgentRuntimeReplacedForTest(true)
	require.True(t, reloaded1.InterruptTaskRunIfRuntimeReplaced(),
		"a load-time replacement ends the in-flight run as interrupted")
	require.False(t, reloaded1.TaskRunActive())
	require.True(t, reloaded1.TaskRunInterrupted())

	// The interrupt persist drops the bound bit (gate-gated) but keeps the
	// stale PromptNotDelivered verdict (unconditional).
	data2 := reloaded1.ToInstanceData()
	require.False(t, data2.TaskRunActive, "the interrupt persist writes TaskRunActive=false")
	require.True(t, data2.TaskRunInterrupted, "the interrupt persist writes TaskRunInterrupted=true")
	require.False(t, data2.TaskRunPromptBound, "the gate drops the bound bit when the run is not active")
	require.Equal(t, PromptNotDelivered, data2.LastPromptDeliveryStatus,
		"the stale verdict rides the record unconditionally")

	// Restart 2: the row loads as interrupted-and-resumable; the resumed
	// agent's in-turn chrome re-arms the run. The re-arm clears the stale
	// delivery verdict so the forced checkpoint does not manufacture the
	// deadly shape.
	data2.BackendType = "docker"
	reloaded2, err := FromInstanceData(data2.ForStorage())
	require.NoError(t, err)
	require.False(t, reloaded2.TaskRunActive(), "the interrupted run loads inactive")
	require.True(t, reloaded2.TaskRunInterrupted(), "the interrupted run loads as interrupted")
	require.False(t, reloaded2.TaskRunFailed(), "the interrupted run is not failed")

	_, resumable := reloaded2.TaskRunResumableSince(time.Now())
	require.True(t, resumable, "the interrupted run is still within the resume window")
	require.True(t, reloaded2.ResumeInterruptedTaskRunAtEpoch(
		time.Now().Add(time.Second), reloaded2.stateEpoch),
		"the agent's in-turn chrome re-arms the run")
	require.True(t, reloaded2.TaskRunActive(), "the run is back in flight")
	require.False(t, reloaded2.TaskRunInterrupted())

	// The forced checkpoint after re-arm must not carry the stale
	// PromptNotDelivered verdict nor its paired attempt timestamp.
	data3 := reloaded2.ToInstanceData()
	require.True(t, data3.TaskRunActive, "the re-armed run is active")
	require.False(t, data3.TaskRunPromptBound, "the boundary is unbound after re-arm")
	require.True(t, data3.TaskRunPromptAttemptAt.IsZero(), "the boundary timestamp is zero after re-arm")
	require.NotEqual(t, PromptNotDelivered, data3.LastPromptDeliveryStatus,
		"the re-arm clears the predecessor-owned delivery verdict")
	require.True(t, data3.LastPromptAttemptAt.IsZero(),
		"the re-arm retires the predecessor-owned attempt timestamp with its verdict")

	// Restart 3: loading the re-armed record must not fire the restart twin.
	data3.BackendType = "docker"
	reloaded3, err := FromInstanceData(data3.ForStorage())
	require.NoError(t, err)
	require.True(t, reloaded3.TaskRunActive(),
		"a resumed live run is not torn down as failed by a stale verdict")
	require.False(t, reloaded3.TaskRunFailed(),
		"the restart twin must not fire for a run whose initial delivery succeeded")
	require.False(t, reloaded3.ConsumeTaskRunFailed(),
		"no failure stamp is armed for a live, resumed run")
	require.True(t, reloaded3.TaskRunPromptAttemptAt().IsZero(),
		"the run's prompt boundary is still zero after the final reload")
}
