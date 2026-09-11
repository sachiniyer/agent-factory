package session

import "time"

// TaskRunIdentity is the durable association between a task delivery and the
// session runtime that received it. TaskGenerationID binds the reusable TaskID
// to one task incarnation; SessionID is the unique run identity; Sequence is its
// clock-independent delivery order; Revision is the task-row CAS observed at
// admission. RunAt is display data and is zero only for records written before
// task-run publication moved inside the manager boundary.
type TaskRunIdentity struct {
	TaskID           string
	TaskGenerationID string
	SessionID        string
	Title            string
	Sequence         uint64
	Revision         uint64
	RunAt            time.Time
	CreatedAt        time.Time
}

// TaskRun returns this session's immutable task-delivery identity. The active
// bit is deliberately separate: a completed session still retains the identity
// that ordered its task-row writes.
func (i *Instance) TaskRun() TaskRunIdentity {
	return TaskRunIdentity{
		TaskID: i.TaskID, TaskGenerationID: i.taskGenerationID,
		SessionID: i.ID, Title: i.Title, Sequence: i.taskRunSequence,
		Revision: i.taskRunRevision, RunAt: i.taskRunAt, CreatedAt: i.CreatedAt,
	}
}

// InterruptTaskRunAtRuntimeReplacement closes a run when the agent runtime that
// received its prompt has been replaced without replaying that prompt. The
// returned identity is valid only when interrupted is true.
//
// Runtime replacement is the provenance proof: restore-time callers invoke this
// at their pre-ConfirmLive boundary, while load reconstructors invoke it only
// after RestoreWithResult confirms RestoreRespawned for the agent tab. A sibling
// tab replacement never reaches it. A prompt-redelivery fence is the explicit
// exception: OpRespawning promises to re-deliver the queued task prompt, while a
// durable OpReplacing mission supplies the replacement agent's continuation
// context. Both keep the run until that delivery transaction lowers its fence.
//
// The daemon must persist the closed session marker before publishing the task
// outcome. ConfirmLive retains runEndsOnRestoredRuntime as a structural fallback
// for restore callers that omit the settlement callback.
func (i *Instance) InterruptTaskRunAtRuntimeReplacement() (TaskRunIdentity, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.interruptTaskRunAtRuntimeReplacementLocked()
}

func (i *Instance) interruptTaskRunAtRuntimeReplacementLocked() (TaskRunIdentity, bool) {
	replaysPrompt := i.inFlightOp == OpRespawning ||
		(i.inFlightOp == OpReplacing && i.pendingHandoffMission != "")
	if replaysPrompt || !i.taskRunActive {
		return TaskRunIdentity{}, false
	}
	i.closeTaskRunLocked()
	i.taskRunInterruptionPending = true
	return i.TaskRun(), true
}

// PendingTaskRunInterruption returns the exact run whose task-row outcome still
// has to be published. The marker is an outbox entry, not a second activity bit:
// taskRunActive is already false while this remains true.
func (i *Instance) PendingTaskRunInterruption() (TaskRunIdentity, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.taskRunInterruptionPending {
		return TaskRunIdentity{}, false
	}
	return i.TaskRun(), true
}

// ClearPendingTaskRunInterruption retires the durable outbox marker only when
// the caller names the same immutable run. A stale retry cannot discharge a
// successor's obligation merely because it retained the Instance pointer.
func (i *Instance) ClearPendingTaskRunInterruption(run TaskRunIdentity) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.taskRunInterruptionPending || run.TaskID != i.TaskID ||
		run.TaskGenerationID != i.taskGenerationID || run.SessionID != i.ID ||
		run.Sequence != i.taskRunSequence || run.Revision != i.taskRunRevision {
		return false
	}
	i.taskRunInterruptionPending = false
	i.touchLocked()
	return true
}

// closeTaskRunLocked performs the shared, one-way task-run close and captures
// the adoption baseline in the same critical section. Caller holds i.mu.
func (i *Instance) closeTaskRunLocked() {
	i.taskRunActive = false
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}
