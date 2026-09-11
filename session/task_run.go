package session

import (
	"strings"
	"time"
)

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
// Runtime replacement is the provenance proof: restore-time backends admit the
// boundary only through ConfirmRuntimeReplacementLive, while load reconstructors
// invoke it only after RestoreWithResult confirms RestoreRespawned for the agent
// tab. A successful reattach and a sibling-tab replacement never reach it. A
// prompt-redelivery fence is the explicit
// exception: OpRespawning promises to re-deliver the queued task prompt, a
// durable OpReplacing mission supplies the replacement agent's continuation
// context, a load-respawned limit-parked task retains its stored queued prompt,
// and a committed manual account swap with transaction-scoped non-delivery proof
// is replayed by the limit scheduler after load. Each keeps the run until that
// delivery transaction settles.
//
// The daemon must persist the closed session marker before publishing the task
// outcome. ConfirmRuntimeReplacementLive retains runEndsOnRestoredRuntime as a
// structural fallback for restore callers that omit the settlement callback.
func (i *Instance) InterruptTaskRunAtRuntimeReplacement() (TaskRunIdentity, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.interruptTaskRunAtRuntimeReplacementLocked()
}

func (i *Instance) interruptTaskRunAtRuntimeReplacementLocked() (TaskRunIdentity, bool) {
	replaysPrompt := i.inFlightOp == OpRespawning ||
		(i.inFlightOp == OpReplacing && i.pendingHandoffMission != "") ||
		(i.liveness == LiveLimitReached && i.TaskID != "" && strings.TrimSpace(i.Prompt) != "") ||
		i.pendingAccountSwapPromptReplayableLocked()
	if replaysPrompt || !i.taskRunActive {
		return TaskRunIdentity{}, false
	}
	i.closeTaskRunLocked()
	i.taskRunInterruptionPending = true
	return i.TaskRun(), true
}

// pendingAccountSwapPromptReplayableLocked identifies the one load-time account
// replacement whose successor is authorized to receive the stored task prompt.
// A marker alone is not enough: manual delivery may be ambiguous, and replaying
// then could duplicate side effects. The transaction must name a mission, prove
// all replacement panes started, and carry its own positive non-delivery verdict.
// Caller holds i.mu.
func (i *Instance) pendingAccountSwapPromptReplayableLocked() bool {
	pending := i.pendingAccountSwap
	return pending != nil && pending.Manual && pending.Mission != "" &&
		pending.ReplacementPanesStarted && pending.MissionDeliveryStatus == PromptNotDelivered
}

// HoldRuntimeReplacementUntilSettlement prevents a restored runtime from
// becoming lifecycle-visible while disk can still resurrect the prompted
// predecessor's active run. It is process-local: the durable fact being awaited
// is the already-set TaskRunInterruptionPending session checkpoint.
func (i *Instance) HoldRuntimeReplacementUntilSettlement() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRestoring || !i.taskRunInterruptionPending {
		return false
	}
	i.runtimeReplacementSettlementBlocked = true
	return true
}

// RuntimeReplacementSettlementBlocked reports whether a failed interrupted-run
// checkpoint owns the restore fence. The settlement retry uses this to bypass
// the ordinary no-write-during-operation rule for this one safe, closed snapshot.
func (i *Instance) RuntimeReplacementSettlementBlocked() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeReplacementSettlementBlocked
}

// ReleaseRuntimeReplacementAfterSettlement exposes a replacement only after a
// successful write made the predecessor run's close durable. It applies
// ConfirmLive under the same lock that removes the hold, so neither a poll nor a
// deferred fence owner can observe an unblocked OpRestoring gap.
func (i *Instance) ReleaseRuntimeReplacementAfterSettlement() (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.runtimeReplacementSettlementBlocked {
		return false, nil
	}
	if i.inFlightOp != OpRestoring {
		// A teardown may supersede recovery while its disk write is failing. That
		// owner now controls visibility; never replace its op with ConfirmLive.
		i.runtimeReplacementSettlementBlocked = false
		return false, nil
	}
	i.runtimeReplacementSettlementBlocked = false
	if err := i.transitionLocked(ConfirmLive()); err != nil {
		i.runtimeReplacementSettlementBlocked = true
		return false, err
	}
	return true, nil
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
