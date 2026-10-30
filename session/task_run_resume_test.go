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
