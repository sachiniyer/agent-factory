package session

import "time"

// IdleReason is the daemon's mechanically established explanation for why a
// session is not doing visible work. It deliberately excludes semantic guesses
// about pane content: a question, a completed task, and a wedged agent can render
// alike, so none of those is a value in this vocabulary.
type IdleReason string

const (
	IdleReasonNone                      IdleReason = ""
	IdleReasonUsageLimit                IdleReason = "usage-limit"
	IdleReasonProcessExited             IdleReason = "process-exited"
	IdleReasonRestoreGaveUp             IdleReason = "restore-gave-up"
	IdleReasonRecreatePending           IdleReason = "recreate-pending"
	IdleReasonPromptNotDelivered        IdleReason = "prompt-not-delivered"
	IdleReasonDeliveryUnconfirmed       IdleReason = "delivery-unconfirmed"
	IdleReasonNoPaneChangeSinceDelivery IdleReason = "no-pane-change-since-delivery"
	IdleReasonSettledAfterPaneChange    IdleReason = "settled-after-pane-change"
)

// Label is the short human wording shared by row renderers. Unknown future
// values render nothing rather than inviting an older client to interpret them.
func (r IdleReason) Label() string {
	switch r {
	case IdleReasonUsageLimit:
		return "usage limit"
	case IdleReasonProcessExited:
		return "process exited"
	case IdleReasonRestoreGaveUp:
		return "restore gave up"
	case IdleReasonRecreatePending:
		return "recreate notice pending"
	case IdleReasonPromptNotDelivered:
		return "prompt not delivered"
	case IdleReasonDeliveryUnconfirmed:
		return "delivery unknown"
	case IdleReasonNoPaneChangeSinceDelivery:
		return "no change after delivery"
	case IdleReasonSettledAfterPaneChange:
		return "pane changed"
	default:
		return ""
	}
}

// IdleReasonFor derives the public reason from closed, mechanically observed
// facts. It does not trust InstanceData.IdleReason: that field is a projection,
// and deriving it here keeps persisted evidence the source of truth.
func IdleReasonFor(data InstanceData) IdleReason {
	if data.InFlightOp != OpNone {
		return IdleReasonNone
	}

	liveness := livenessFromData(data)
	switch liveness {
	case LiveLimitReached:
		return IdleReasonUsageLimit
	case LiveLost, LiveDead:
		if data.LostRestoreFailure != nil && data.LostRestoreFailure.valid() {
			return IdleReasonRestoreGaveUp
		}
		return IdleReasonProcessExited
	case LiveReady:
		// The prompt/recreate evidence below only explains a settled Ready row.
	default:
		return IdleReasonNone
	}

	switch data.RootRecreateContext {
	case RootRecreateContextFresh, RootRecreateContextUnknown:
		return IdleReasonRecreatePending
	}

	if data.LastPromptAttemptAt.IsZero() {
		return IdleReasonNone
	}
	if data.LastPromptDeliveryStatus == PromptNotDelivered {
		return IdleReasonPromptNotDelivered
	}
	switch data.LastPromptDeliveryStatus {
	case PromptSentUnverified, PromptCouldNotConfirm:
		if data.LastPaneChurnAt.After(data.LastPromptAttemptAt) {
			return IdleReasonSettledAfterPaneChange
		}
		return IdleReasonDeliveryUnconfirmed
	case PromptDelivered:
		if data.LastPaneChurnAt.After(data.LastPromptAttemptAt) {
			return IdleReasonSettledAfterPaneChange
		}
		return IdleReasonNoPaneChangeSinceDelivery
	default:
		return IdleReasonNone
	}
}

// ProjectIdleReason recomputes the projection field from its evidence. It is
// used by daemonless disk-list fallback as well as live Instance snapshots.
func (d InstanceData) ProjectIdleReason() InstanceData {
	d.IdleReason = IdleReasonFor(d)
	return d
}

// WithoutIdleEvidence returns a checkpoint that cannot attribute observations
// from a retired runtime to its replacement.
func (d InstanceData) WithoutIdleEvidence() InstanceData {
	d.IdleReason = IdleReasonNone
	d.LastPromptAttemptAt = time.Time{}
	d.LastPromptDeliveryStatus = ""
	d.LastPaneChurnAt = time.Time{}
	d.TaskRunTurnObservedAt = time.Time{}
	return d
}

// RecordPromptAttempt stores the observation made by an actual prompt send.
// attemptedAt must be captured before delivery begins, so a pane observation
// racing the send can still be ordered after it. Invalid local values normalize
// to honest uncertainty; a zero timestamp establishes no order and is ignored.
func (i *Instance) RecordPromptAttempt(status PromptDeliveryStatus, attemptedAt time.Time) bool {
	if attemptedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.recordPromptAttemptLocked(status, attemptedAt)
}

// recordPromptAttemptForObservation commits only while runtime still owns the
// delivery. A replacement can proceed without waiting for predecessor I/O; a
// send that returns afterwards must not seed evidence onto the successor.
func (i *Instance) recordPromptAttemptForObservation(
	status PromptDeliveryStatus,
	attemptedAt time.Time,
	runtime *agentObservationRuntime,
) bool {
	if attemptedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agentObservation != runtime {
		return false
	}
	return i.recordPromptAttemptLocked(status, attemptedAt)
}

// recordPromptAttemptLocked stores a prompt boundary. Caller holds i.mu.
func (i *Instance) recordPromptAttemptLocked(status PromptDeliveryStatus, attemptedAt time.Time) bool {
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	if i.lastPromptAttemptAt.Equal(attemptedAt) && i.lastPromptDeliveryStatus == status {
		return false
	}
	// The attempt and pane snapshots share their runtime's observation lock, while stateEpoch
	// fences the apply after Snapshot returns. Retire a churn timestamp applied by
	// an observation that completed immediately before this attempt acquired that
	// mutex: its capture predates delivery even if its apply timestamp does not.
	if !i.lastPaneChurnAt.IsZero() && !i.lastPaneChurnAt.Before(attemptedAt) {
		i.lastPaneChurnAt = time.Time{}
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	i.touchLocked()
	i.stateEpoch++
	return true
}

// RecordPaneChurnAtEpoch records that an Observation reported Updated for the
// same runtime generation the caller observed. A lifecycle fence raised during
// capture invalidates the observation exactly as it invalidates liveness.
func (i *Instance) RecordPaneChurnAtEpoch(churnAt time.Time, observedEpoch uint64) bool {
	recorded, _ := i.RecordPaneChurnCheckpointAtEpoch(churnAt, observedEpoch)
	return recorded
}

// RecordPaneChurnCheckpointAtEpoch additionally reports the first accepted pane
// churn after the latest prompt. That edge must be persisted even while the row
// remains Running; later spinner churn needs no write on every poll.
func (i *Instance) RecordPaneChurnCheckpointAtEpoch(churnAt time.Time, observedEpoch uint64) (bool, bool) {
	if churnAt.IsZero() {
		return false, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch || !churnAt.After(i.lastPaneChurnAt) {
		return false, false
	}
	checkpoint := !i.lastPromptAttemptAt.IsZero() &&
		churnAt.After(i.lastPromptAttemptAt) &&
		!i.lastPaneChurnAt.After(i.lastPromptAttemptAt)
	i.lastPaneChurnAt = churnAt
	i.touchLocked()
	return true, checkpoint
}

// taskRunCompletionQuietGrace is how long the pane must sit unchanged after
// post-attempt churn before that churn may stand in for turn evidence. Boot
// and echo output arrives in bursts — devin's prompt echo lands seconds after
// Enter, then ACP init and skill discovery — so only sustained silence after
// the last observed output counts. It is the release arm for agents whose
// pane exposes no in-turn signature (aider, gemini, program overrides) and for
// turns too fast for two poll captures to catch the timer moving. A var so
// tests can compress it.
var taskRunCompletionQuietGrace = 30 * time.Second

// RecordTaskRunTurn records that the agent's own in-turn chrome was observed
// on this runtime — the positive "a turn on the prompt began" evidence a
// booting or echoing pane cannot produce (#5219). The unfenced form for the
// send path: the post-submit watch sees the chrome synchronously inside
// delivery, so there is no snapshot epoch to fence against.
func (i *Instance) RecordTaskRunTurn(observedAt time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.taskRunActive || !observedAt.After(i.taskRunTurnObservedAt) {
		return false
	}
	i.taskRunTurnObservedAt = observedAt
	i.touchLocked()
	return true
}

// RecordTaskRunTurnAtEpoch is the epoch-fenced form for the status poll: an
// observation captured before a lifecycle fence must not apply after it,
// exactly as RecordPaneChurnAtEpoch applies pane churn.
func (i *Instance) RecordTaskRunTurnAtEpoch(observedAt time.Time, observedEpoch uint64) bool {
	if observedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.taskRunActive || i.stateEpoch != observedEpoch || !observedAt.After(i.taskRunTurnObservedAt) {
		return false
	}
	i.taskRunTurnObservedAt = observedAt
	i.touchLocked()
	return true
}

// taskRunAwaitingTurnLocked reports whether the session's prompt has been
// attempted but the agent has not demonstrably taken the turn (#5219). Mere
// post-attempt churn is not that boundary — a still-booting pane produces it
// on its own. What releases the gate is the agent's own in-turn chrome
// observed after the send, or a post-attempt burst followed by silence longer
// than the completion grace. An attempt timestamp alone arms the gate however
// the send went: a run that can show no turn is not a completed run, only an
// idle pane.
//
// Caller holds i.mu.
func (i *Instance) taskRunAwaitingTurnLocked() bool {
	if i.lastPromptAttemptAt.IsZero() {
		return false
	}
	if i.taskRunTurnObservedAt.After(i.lastPromptAttemptAt) {
		return false
	}
	if i.lastPaneChurnAt.After(i.lastPromptAttemptAt) &&
		time.Since(i.lastPaneChurnAt) >= taskRunCompletionQuietGrace {
		return false
	}
	return true
}

// ClearIdleEvidence retires delivery and pane facts owned by a replaced runtime.
// Its epoch bump also rejects a predecessor observation still applying.
func (i *Instance) ClearIdleEvidence() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := !i.lastPromptAttemptAt.IsZero() || i.lastPromptDeliveryStatus != "" ||
		!i.lastPaneChurnAt.IsZero() || !i.taskRunTurnObservedAt.IsZero()
	i.lastPromptAttemptAt = time.Time{}
	i.lastPromptDeliveryStatus = ""
	i.lastPaneChurnAt = time.Time{}
	i.taskRunTurnObservedAt = time.Time{}
	// A predecessor snapshot may still be blocked in transport I/O. Rotate the
	// serialization domain instead of making replacement delivery wait for it;
	// the generation invalidation fences daemon-owned side effects while the epoch
	// bump below rejects instance state applied after that snapshot returns.
	i.agentObservationGeneration.Add(1)
	i.agentObservation = nil
	i.stateEpoch++
	if changed {
		i.touchLocked()
	}
	return changed
}

// markLoadRuntimeReplaced records that Start(false) created a replacement
// process, retired an unverified reattachment's persisted runtime command, or
// recorded a process tab's observed exit (#4506 review). The daemon loader
// consumes this after FromInstanceData returns so the timestamp, evidence
// clear or exit stamp is checkpointed before the row is installed. Marking a
// sibling replacement does not clear agent evidence.
func (i *Instance) markLoadRuntimeReplaced() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.loadRuntimeReplaced = true
}

// ConsumeLoadRuntimeReplacement reports one load-time replacement exactly once.
// It is process-local coordination, never a persisted fact about the session.
func (i *Instance) ConsumeLoadRuntimeReplacement() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	replaced := i.loadRuntimeReplaced
	i.loadRuntimeReplaced = false
	return replaced
}

// ReconcileIdleEvidence mirrors the daemon's evidence onto a client row model.
// It applies both directions because runtime replacement can clear or replace
// the evidence, and the daemon snapshot is authoritative for all four fields.
func (i *Instance) ReconcileIdleEvidence(attemptedAt time.Time, status PromptDeliveryStatus, churnAt, turnAt time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastPromptAttemptAt.Equal(attemptedAt) &&
		i.lastPromptDeliveryStatus == status &&
		i.lastPaneChurnAt.Equal(churnAt) &&
		i.taskRunTurnObservedAt.Equal(turnAt) {
		return false
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	i.lastPaneChurnAt = churnAt
	i.taskRunTurnObservedAt = turnAt
	i.touchLocked()
	return true
}

// taskRunTurnObservedAtFromData gates a persisted turn observation on the run
// still being in flight, mirroring ToInstanceData's write-side gate: a finished
// run's stale timestamp is not evidence for its successor's delivery window.
func taskRunTurnObservedAtFromData(data InstanceData) time.Time {
	if !data.TaskRunActive {
		return time.Time{}
	}
	return data.TaskRunTurnObservedAt
}

// IdleReasonSnapshot returns the derived reason and last observed pane churn in
// one lock hold for row renderers.
func (i *Instance) IdleReasonSnapshot() (IdleReason, time.Time) {
	reason, _, churnAt := i.IdleReasonDetailSnapshot()
	return reason, churnAt
}

// IdleReasonDetailSnapshot adds the structured terminal restore failure needed
// by row renderers, under the same lock as the derived reason.
func (i *Instance) IdleReasonDetailSnapshot() (IdleReason, *LostRestoreFailure, time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	data := InstanceData{
		Status:                   i.statusLocked(),
		Liveness:                 i.liveness,
		InFlightOp:               i.inFlightOp,
		LostRestoreFailure:       cloneLostRestoreFailure(i.lostRestoreFailure),
		RootRecreateContext:      i.rootRecreateContext,
		LastPromptAttemptAt:      i.lastPromptAttemptAt,
		LastPromptDeliveryStatus: i.lastPromptDeliveryStatus,
		LastPaneChurnAt:          i.lastPaneChurnAt,
	}
	return IdleReasonFor(data), cloneLostRestoreFailure(i.lostRestoreFailure), i.lastPaneChurnAt
}
