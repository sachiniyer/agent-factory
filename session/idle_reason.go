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

	attemptAt, status, churnAt :=
		data.LastPromptAttemptAt, data.LastPromptDeliveryStatus, data.LastPaneChurnAt
	if attemptAt.IsZero() && data.TaskRunActive && !data.TaskRunPromptAttemptAt.IsZero() {
		// The run's own boundary survives a runtime replacement while the
		// pane-relative session evidence does not — derive the reason from the
		// gate still holding the row, or a retained PromptNotDelivered would
		// sit Ready consuming its slot with no diagnostic at all (#5221 review).
		attemptAt, status, churnAt =
			data.TaskRunPromptAttemptAt, data.TaskRunPromptDeliveryStatus, time.Time{}
	}
	if attemptAt.IsZero() {
		return IdleReasonNone
	}
	if status == PromptNotDelivered {
		return IdleReasonPromptNotDelivered
	}
	switch status {
	case PromptSentUnverified, PromptCouldNotConfirm:
		if churnAt.After(attemptAt) {
			return IdleReasonSettledAfterPaneChange
		}
		return IdleReasonDeliveryUnconfirmed
	case PromptDelivered:
		if churnAt.After(attemptAt) {
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
// from a retired runtime to its replacement. The task-run fields are NOT
// scrubbed: they describe the run, not the pane, and dropping them would leave
// a restored active run without its delivery gate — the #5219 bug returning
// through the crash-checkpoint path (#5221 review).
func (d InstanceData) WithoutIdleEvidence() InstanceData {
	d.IdleReason = IdleReasonNone
	d.LastPromptAttemptAt = time.Time{}
	d.LastPromptDeliveryStatus = ""
	d.LastPaneChurnAt = time.Time{}
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
	return i.recordPromptAttemptLocked(status, attemptedAt, false)
}

// RecordTaskRunPromptAttempt is the task-scoped form: the send speaks for the
// task's own machinery (the run's prompt, a handoff or account-swap mission,
// a limit-resume resend), so it may re-arm a satisfied window (#5221 review).
func (i *Instance) RecordTaskRunPromptAttempt(status PromptDeliveryStatus, attemptedAt time.Time) bool {
	if attemptedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.recordPromptAttemptLocked(status, attemptedAt, true)
}

// recordPromptAttemptForObservation commits only while runtime still owns the
// delivery. A replacement can proceed without waiting for predecessor I/O; a
// send that returns afterwards must not seed evidence onto the successor.
func (i *Instance) recordPromptAttemptForObservation(
	status PromptDeliveryStatus,
	attemptedAt time.Time,
	runtime *agentObservationRuntime,
	taskScoped bool,
) bool {
	if attemptedAt.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agentObservation != runtime {
		return false
	}
	return i.recordPromptAttemptLocked(status, attemptedAt, taskScoped)
}

// recordPromptAttemptLocked stores a prompt boundary. rearmTaskRun marks a
// send that speaks for the task itself — the task's own prompt, a handoff or
// account-swap mission, a limit-resume resend — which may re-arm even a
// satisfied window because the machinery just asked for NEW work (#5221
// review). An operator's manual send never carries it: once the run's turn was
// taken, interactive prompts are the user's business, not the task's. Caller
// holds i.mu.
func (i *Instance) recordPromptAttemptLocked(status PromptDeliveryStatus, attemptedAt time.Time, rearmTaskRun bool) bool {
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
	// The task run's completion gate binds to the run's own prompt boundary,
	// not the session's latest send (#5221 review): a manual prompt after the
	// agent already took the turn must not re-arm a window that was satisfied —
	// a failed poke would otherwise hold the run open forever waiting for churn
	// the dead send cannot produce. Sends DO still move the boundary while the
	// window is unsatisfied: a redelivery is the one way out of a window whose
	// prompt was affirmatively not delivered. The boundary is the last send
	// that could actually have delivered — a PromptNotDelivered result proves
	// the pane took nothing, so it must not supersede a standing boundary and
	// strand the window unsatisfiable.
	if i.taskRunActive &&
		(i.taskRunPromptAttemptAt.IsZero() ||
			(status != PromptNotDelivered &&
				(rearmTaskRun || !i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt)))) {
		i.taskRunPromptAttemptAt = attemptedAt
		i.taskRunPromptDeliveryStatus = status
		// The silent grace is measured from each armed boundary — and restarted
		// by runtime replacement (ClearIdleEvidence) — never from the run's
		// original send, or a recovered pane would inherit a spent deadline
		// (#5221 review).
		i.taskRunSilentBaseAt = attemptedAt
	}
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

// taskRunCompletionSilentGrace bounds the case the churn arm cannot reach: a
// signature-less agent (or program override) whose whole turn fits between two
// poll captures and leaves the pane byte-identical produces neither in-turn
// chrome NOR post-attempt churn, so the quiet arm's churn requirement could
// never be satisfied and the run would hold its concurrency slot forever
// (#5221 review). The fallback for that shape is time alone, and it must be a
// DIFFERENT order of magnitude from the quiet arm's: a still-booting agent can
// sit silent well past 30s, so only a grace long enough to outlast a real
// boot may stand in for evidence. A var so tests can compress it.
var taskRunCompletionSilentGrace = 5 * time.Minute

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
	return i.recordTaskRunTurnLocked(observedAt)
}

// recordTaskRunTurnLocked latches the FIRST post-boundary chrome observation:
// once the stored stamp already satisfies the window, further ticking rows are
// the same turn continuing, not new evidence — accepting them would checkpoint
// the whole instances file on every poll while the agent stays in turn (#5221
// review). A re-armed boundary (a redelivery while unsatisfied) sits after the
// latched stamp, so a new window still takes its first observation.
//
// Caller holds i.mu.
func (i *Instance) recordTaskRunTurnLocked(observedAt time.Time) bool {
	if !i.taskRunActive || i.taskRunPromptAttemptAt.IsZero() ||
		i.taskRunPromptDeliveryStatus == PromptNotDelivered ||
		!observedAt.After(i.taskRunTurnObservedAt) ||
		i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt) {
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
	if i.stateEpoch != observedEpoch {
		return false
	}
	return i.recordTaskRunTurnLocked(observedAt)
}

// taskRunAwaitingTurnLocked reports whether the run's prompt has been
// attempted but the agent has not demonstrably taken the turn (#5219). The
// boundary is the run's OWN prompt evidence — taskRunPromptAttemptAt — not
// the session's latest send, so an interactive prompt cannot re-arm a window
// the agent already satisfied (#5221 review). Mere post-attempt churn is not
// the release either — a still-booting pane produces it on its own. What
// releases the gate is the agent's own in-turn chrome observed after the
// send, or a post-attempt burst followed by silence longer than the
// completion grace. A send that affirmatively failed delivery arms an
// unsatisfiable window: churn after it cannot be a turn on a prompt that
// never landed, so the run stays open and flagged prompt-not-delivered until
// a redelivery re-arms the boundary.
//
// Caller holds i.mu.
func (i *Instance) taskRunAwaitingTurnLocked() bool {
	if i.taskRunPromptAttemptAt.IsZero() {
		return false
	}
	if i.taskRunPromptDeliveryStatus == PromptNotDelivered {
		return true
	}
	if i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt) {
		return false
	}
	return !i.taskRunQuietReleaseLocked() && !i.taskRunSilentReleaseLocked()
}

// taskRunQuietReleaseLocked reports whether the completion gate is satisfied
// by the fallback arm alone: post-attempt pane churn followed by silence
// longer than the completion grace. Satisfied here never means the agent
// demonstrably took the turn — only that a boot or echo burst had time to
// finish. The transition log names this arm distinctly so a run completed on
// it reads differently from one released by in-turn chrome (#5219).
//
// Caller holds i.mu.
func (i *Instance) taskRunQuietReleaseLocked() bool {
	return i.lastPaneChurnAt.After(i.taskRunPromptAttemptAt) &&
		time.Since(i.lastPaneChurnAt) >= taskRunCompletionQuietGrace
}

// taskRunSilentReleaseLocked is the bounded release for a turn the poll can
// miss completely: no chrome, and not even churn — the pane never changed
// after the attempt. It measures from the ATTEMPT, not the last churn, and at
// a grace an order of magnitude past the quiet arm so a slow boot cannot ride
// it out (#5221 review).
//
// Caller holds i.mu.
func (i *Instance) taskRunSilentReleaseLocked() bool {
	if i.lastPaneChurnAt.After(i.taskRunPromptAttemptAt) {
		// Post-boundary churn means the pane IS answering — that run belongs to
		// the quiet arm, which demands its own 30s of silence after the last
		// output. The silent arm is only for a pane that never moved at all.
		return false
	}
	base := i.taskRunSilentBaseAt
	if base.IsZero() {
		base = i.taskRunPromptAttemptAt
	}
	return time.Since(base) >= taskRunCompletionSilentGrace
}

// ClearIdleEvidence retires delivery and pane facts owned by a replaced runtime.
// Its epoch bump also rejects a predecessor observation still applying.
func (i *Instance) ClearIdleEvidence() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := !i.lastPromptAttemptAt.IsZero() || i.lastPromptDeliveryStatus != "" ||
		!i.lastPaneChurnAt.IsZero()
	i.lastPromptAttemptAt = time.Time{}
	i.lastPromptDeliveryStatus = ""
	i.lastPaneChurnAt = time.Time{}
	// The task-run fields stay: they are facts about the RUN, not the pane —
	// the prompt was still attempted and any turn observation still stands. A
	// replacement runtime boots behind the same gate (#5219): were the boundary
	// cleared here, its first quiet Ready tick would end a run whose turn never
	// visibly began — the original bug through the recovery path (#5221 review).
	// The churn/quiet evidence above is pane-relative and must still reset.
	// The SILENT grace restarts with the new runtime though — it exists for a
	// pane that never moved, and a fresh pane has not had its own window yet.
	if i.taskRunActive && !i.taskRunPromptAttemptAt.IsZero() {
		i.taskRunSilentBaseAt = time.Now()
		changed = true
	}
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
// the evidence, and the daemon snapshot is authoritative for all six fields.
func (i *Instance) ReconcileIdleEvidence(attemptedAt time.Time, status PromptDeliveryStatus, churnAt,
	taskAttemptAt time.Time, taskStatus PromptDeliveryStatus, turnAt, silentBaseAt time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastPromptAttemptAt.Equal(attemptedAt) &&
		i.lastPromptDeliveryStatus == status &&
		i.lastPaneChurnAt.Equal(churnAt) &&
		i.taskRunPromptAttemptAt.Equal(taskAttemptAt) &&
		i.taskRunPromptDeliveryStatus == taskStatus &&
		i.taskRunTurnObservedAt.Equal(turnAt) &&
		i.taskRunSilentBaseAt.Equal(silentBaseAt) {
		return false
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	i.lastPaneChurnAt = churnAt
	i.taskRunPromptAttemptAt = taskAttemptAt
	i.taskRunPromptDeliveryStatus = taskStatus
	i.taskRunTurnObservedAt = turnAt
	i.taskRunSilentBaseAt = silentBaseAt
	i.touchLocked()
	return true
}

// ReconcileTaskRunState mirrors the daemon's authoritative run marker and hold
// flags. A client that missed the snapshot carrying turn evidence cannot
// re-derive the run end locally — the final Ready snapshot scrubs the window
// fields with the run — so the marker itself must be mirrored rather than
// recomputed from evidence that is no longer there (#5221 review).
func (i *Instance) ReconcileTaskRunState(active, missionHeld, turnGateHeld bool) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.taskRunActive == active && i.taskRunIdleEdgeHeld == missionHeld &&
		i.taskRunTurnGateHeld == turnGateHeld {
		return false
	}
	i.taskRunActive = active
	i.taskRunIdleEdgeHeld = missionHeld
	i.taskRunTurnGateHeld = turnGateHeld
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

// taskRunPromptBoundaryFromData is the same restore-side gate for the run's
// own prompt boundary: evidence written for a finished run carries no window.
// A row persisted by a release that predates the task-scoped fields keeps an
// active run with only the session-level send on record — that send IS the
// task's prompt for an upgrade-era row, so adopt it rather than restart with
// an unarmed gate that lets the first Ready tick complete the run (#5221).
func taskRunPromptBoundaryFromData(data InstanceData) (time.Time, PromptDeliveryStatus) {
	if !data.TaskRunActive {
		return time.Time{}, ""
	}
	if data.TaskRunPromptAttemptAt.IsZero() {
		// The adopted send is the SESSION's latest, not provably the task's —
		// a manual `af sessions send` after the real turn may have overwritten
		// it. A failed send is the ambiguous case that must not migrate as-is:
		// PromptNotDelivered is unsatisfiable, so adopting it would wedge the
		// concurrency slot forever on evidence that may have nothing to do with
		// the task's own prompt. Coerce to unverified — still armed, still
		// releaseable by chrome or the quiet fallback (#5221 review).
		status := data.LastPromptDeliveryStatus
		if status == PromptNotDelivered {
			status = PromptSentUnverified
		}
		return data.LastPromptAttemptAt, status
	}
	return data.TaskRunPromptAttemptAt, data.TaskRunPromptDeliveryStatus
}

// taskRunSilentBaseFromData restores the silent grace's measuring point for an
// active run: the persisted base when present, else the adopted boundary —
// the value pre-field records behaved as (#5221 review).
func taskRunSilentBaseFromData(data InstanceData, boundary time.Time) time.Time {
	if !data.TaskRunActive {
		return time.Time{}
	}
	if data.TaskRunSilentBaseAt.IsZero() {
		return boundary
	}
	return data.TaskRunSilentBaseAt
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
		// The task-scoped fields feed IdleReasonFor's gate fallback: after a
		// runtime replacement the session-level evidence is gone but the run's
		// own boundary still holds — its status is the row's only diagnostic
		// (#5221 review).
		TaskRunActive:               i.taskRunActive,
		TaskRunPromptAttemptAt:      i.taskRunPromptAttemptAt,
		TaskRunPromptDeliveryStatus: i.taskRunPromptDeliveryStatus,
	}
	return IdleReasonFor(data), cloneLostRestoreFailure(i.lostRestoreFailure), i.lastPaneChurnAt
}
