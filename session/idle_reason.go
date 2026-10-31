package session

import "time"

// IdleReason is the daemon's mechanically established explanation for why a
// session is not doing visible work. It deliberately excludes semantic guesses
// about pane content: a question, a completed task, and a wedged agent can render
// alike, so none of those is a value in this vocabulary.
type IdleReason string

const (
	IdleReasonNone                        IdleReason = ""
	IdleReasonUsageLimit                  IdleReason = "usage-limit"
	IdleReasonProcessExited               IdleReason = "process-exited"
	IdleReasonRestoreGaveUp               IdleReason = "restore-gave-up"
	IdleReasonRestoreWaitingOnEnvironment IdleReason = "restore-waiting-on-environment"
	IdleReasonRecreatePending             IdleReason = "recreate-pending"
	IdleReasonPromptNotDelivered          IdleReason = "prompt-not-delivered"
	IdleReasonDeliveryUnconfirmed         IdleReason = "delivery-unconfirmed"
	IdleReasonNoPaneChangeSinceDelivery   IdleReason = "no-pane-change-since-delivery"
	IdleReasonSettledAfterPaneChange      IdleReason = "settled-after-pane-change"
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
	case IdleReasonRestoreWaitingOnEnvironment:
		return "restore waiting on environment"
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
		// A kill tombstone permanently disables automatic restore, so a
		// persisted wait on a tombstoned row is an armed retry nothing runs —
		// exclude it even when the record somehow still carries one (#5295
		// review).
		if !data.UserKilled && data.LostRestoreWait != nil && data.LostRestoreWait.valid() {
			return IdleReasonRestoreWaitingOnEnvironment
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
// from a retired runtime to its replacement.
func (d InstanceData) WithoutIdleEvidence() InstanceData {
	d.IdleReason = IdleReasonNone
	d.LastPromptAttemptAt = time.Time{}
	d.LastPromptDeliveryStatus = ""
	d.LastPaneChurnAt = time.Time{}
	d.TaskRunPromptAttemptAt = time.Time{}
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
	// The task run's completion gate binds to the run's own prompt boundary,
	// not the session's latest send (#5221 review): a manual prompt after the
	// agent already took the turn must not re-arm a window that was satisfied.
	// Sends DO still move the boundary while the window is unsatisfied — a
	// redelivery is how a pending window starts fresh. A send that
	// affirmatively failed delivery never binds it at all: the pane provably
	// took nothing, so the boundary stays the last send that could have
	// delivered.
	if i.runGateActiveLocked() && status != PromptNotDelivered &&
		(i.taskRunPromptAttemptAt.IsZero() ||
			!i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt)) {
		i.taskRunPromptAttemptAt = attemptedAt
		i.taskRunPromptBound = true
	}
	// A send that affirmatively failed while the run's boundary was still
	// unbound is the run's INITIAL delivery proven undelivered (#5278): the
	// prompt demonstrably never reached the pane and no earlier send could
	// have delivered it. The run ends here, failed — left open, its unarmed
	// gate lets the first idle edge read as a completion and hand on_complete
	// a session that never received its prompt.
	//
	// The boundary must also have NEVER bound (#5644): a replacement mission's
	// failed send after a handoff lands on a boundary the swap cleared, though
	// the original prompt provably landed — that run is not this failure.
	if status == PromptNotDelivered && i.taskRunActive &&
		i.taskRunPromptAttemptAt.IsZero() && !i.taskRunPromptBound {
		i.endTaskRunFailedLocked()
		i.taskRunFailedStamp = true
		i.taskRunFailurePending = true
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
	if !i.runGateActiveLocked() || i.taskRunPromptAttemptAt.IsZero() ||
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
// send, a post-attempt burst followed by silence longer than the completion
// grace, or — for a turn the poll can miss completely — the bounded silent
// arm.
//
// Caller holds i.mu.
func (i *Instance) taskRunAwaitingTurnLocked() bool {
	if i.taskRunPromptAttemptAt.IsZero() {
		return false
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
	if !i.lastPaneChurnAt.After(i.taskRunPromptAttemptAt) {
		return false
	}
	// The durable churn stamp can be far older than the pane's true last
	// output — churn after the first post-prompt edge is not re-checkpointed —
	// so silence is measured from the later of the stamp and the START of the
	// current uninterrupted capture streak. A dead or never-born streak means
	// nothing has been observed since (re)start or since the last capture
	// aged out, and a hole mid-window means the churn it hid resets the
	// clock: unobserved time is never silence (#5221 review).
	if !i.paneObservationLiveLocked() {
		return false
	}
	base := i.lastPaneChurnAt
	if i.paneObservationStreakAt.After(base) {
		base = i.paneObservationStreakAt
	}
	return time.Since(base) >= taskRunCompletionQuietGrace
}

// taskRunSilentReleaseLocked is the bounded release for a turn the poll can
// miss completely: no chrome, and not even churn — the pane never changed
// after the attempt. It measures from the later of the ATTEMPT and the START
// of the current uninterrupted observation streak — elapsed wall time is not
// silence, and a hole (restart materialization, failed probes, a stalled
// loop) cannot complete the window because whatever it hid is precisely the
// churn this arm exists to catch (#5221 review) — at a grace an order of
// magnitude past the quiet arm so a slow boot cannot ride it out.
//
// Caller holds i.mu.
func (i *Instance) taskRunSilentReleaseLocked() bool {
	if i.lastPaneChurnAt.After(i.taskRunPromptAttemptAt) {
		// Post-boundary churn means the pane IS answering — that run belongs to
		// the quiet arm, which demands its own 30s of silence after the last
		// output. The silent arm is only for a pane that never moved at all.
		return false
	}
	if !i.paneObservationLiveLocked() {
		return false
	}
	base := i.taskRunPromptAttemptAt
	if i.paneObservationStreakAt.After(base) {
		base = i.paneObservationStreakAt
	}
	return time.Since(base) >= taskRunCompletionSilentGrace
}

// paneObservationLiveLocked reports whether the observation streak is still
// current — a confirmed capture landed within the gap slack its caller
// declared. A run of failed or swallowed captures lets it lapse, and the
// arms then hold rather than inheriting a satisfied window from a streak the
// pane stopped confirming (#5221 review).
//
// Caller holds i.mu.
func (i *Instance) paneObservationLiveLocked() bool {
	return !i.paneObservationExpiresAt.IsZero() && time.Now().Before(i.paneObservationExpiresAt)
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
	// The task-run boundary and turn observation are pane-relative evidence of
	// the RETIRED runtime: a replacement pane owns neither the send it did not
	// make nor the chrome it did not show, so the delivery window does not
	// carry across the swap — the run's remaining life follows the pane the
	// same way it did before the gate existed (#5221 review).
	if !i.taskRunPromptAttemptAt.IsZero() || !i.taskRunTurnObservedAt.IsZero() {
		i.taskRunPromptAttemptAt = time.Time{}
		i.taskRunTurnObservedAt = time.Time{}
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

// markAgentRuntimeReplaced records whether the runtime a recover or load just
// re-established is a REPLACEMENT process — a respawn or remote re-provision,
// or a reattach whose pane identity could not be verified — rather than the
// recorded agent still running (#4222). Both branches of a re-establishment
// assign it before their ConfirmLive edge (or before Start(false) returns), so
// the run-end classifier reads this attempt's verdict, never a stale one.
func (i *Instance) markAgentRuntimeReplaced(replaced bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.agentRuntimeReplaced = replaced
	// Remember WHEN, so an interruption's boundary is the launch, not the later
	// settle edge: an agent already mid-turn at ConfirmLive started after this (#6231).
	if replaced {
		i.agentReplacedAt = time.Now()
	} else {
		i.agentReplacedAt = time.Time{}
	}
}

// InterruptTaskRunIfRuntimeReplaced ends a still-in-flight task run as
// interrupted when this instance's runtime was just established as a
// replacement (#4222). It is the load path's sibling of transition.go's
// ConfirmLive carve-out — there is no ConfirmLive edge where Start(false)
// re-spawns a gone runtime, so the daemon's load settlement calls this before
// persisting the row. Reports whether a run was ended.
func (i *Instance) InterruptTaskRunIfRuntimeReplaced() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.taskRunActive || !i.agentRuntimeReplaced {
		return false
	}
	i.endTaskRunInterruptedLocked()
	return true
}

// InterruptTaskRunForRestoreBoundary ends a still-in-flight task run as
// interrupted at a restore's ConfirmLive boundary — BEFORE the daemon's
// settlement persist — when the runtime being confirmed is a replacement and
// the fence it confirms is OpRestoring (#4222 review). The op scope keeps a
// limit-resume's OpRespawning boundary out: that path replays the prompt, so
// its run is alive by design. Landing the end inside the boundary write is
// what makes it durable through a daemon exit between that settlement and the
// post-recovery persist — a restart would otherwise reattach to the
// replacement's own verified identity and let its first idle edge complete
// the run for on_complete. Reports whether a run was ended; the one-shot
// stamp flag is armed either way, so the post-recovery settlement records the
// outcome exactly once.
func (i *Instance) InterruptTaskRunForRestoreBoundary() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.taskRunActive || !i.agentRuntimeReplaced || i.inFlightOp != OpRestoring {
		return false
	}
	i.endTaskRunInterruptedLocked()
	i.taskRunInterruptedStamp = true
	return true
}

// TaskRunInterrupted reports whether this session's task run ended interrupted
// by a runtime replacement (#4222) rather than on an agent-idle completion
// edge. Sticky for the process's lifetime — used by the daemon's completion
// predicate to tell the two ways the run marker clears apart.
func (i *Instance) TaskRunInterrupted() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.taskRunInterrupted
}

// taskRunResumeWindow bounds how long after an interruption the agent's own
// in-turn chrome re-arms the run (#6223). A restored agent that resumes its
// conversation starts its turn within moments; a human prompting the session
// much later is not the task's run and must not be archived by on_complete.
const taskRunResumeWindow = 15 * time.Minute

// TaskRunObservable reports whether the daemon must observe this session's
// run: a task run or a backlog run in flight (both use the task run's completion
// gate), or a task run interrupted and still resumable (#6230). Every gate that
// decides whether to watch a run asks this, never TaskRunActive alone.
func (i *Instance) TaskRunObservable(now time.Time) bool {
	if i.RunGateActive() {
		return true
	}
	_, ok := i.TaskRunResumableSince(now)
	return ok
}

// TaskRunResumableSince reports whether an interrupted run may still be resumed
// by in-turn evidence, and the boundary that evidence must follow (#6223).
func (i *Instance) TaskRunResumableSince(now time.Time) (time.Time, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.taskRunActive || !i.taskRunInterrupted || i.taskRunInterruptedAt.IsZero() ||
		now.Sub(i.taskRunInterruptedAt) > taskRunResumeWindow {
		return time.Time{}, false
	}
	return i.taskRunInterruptedAt, true
}

// ResumeInterruptedTaskRunAtEpoch re-arms a run that a restore ended as
// interrupted, on proof that the agent itself is in a turn after the
// interruption (#6223). #4222 ended the run so a REPLACEMENT'S first idle tick
// could not read as completion; an agent that resumes its conversation and works
// is the run continuing, and its next idle edge must complete it, or on_complete
// is orphaned. The held-edge marker makes any later idle observation end it.
func (i *Instance) ResumeInterruptedTaskRunAtEpoch(observedAt time.Time, observedEpoch uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch || i.taskRunActive || !i.taskRunInterrupted ||
		i.taskRunInterruptedAt.IsZero() || !observedAt.After(i.taskRunInterruptedAt) ||
		observedAt.Sub(i.taskRunInterruptedAt) > taskRunResumeWindow {
		return false
	}
	i.taskRunActive = true
	i.taskRunInterrupted = false
	i.taskRunInterruptedAt = time.Time{}
	i.taskRunIdleEdgeHeld = true
	i.taskRunPromptAttemptAt = time.Time{}
	i.taskRunTurnObservedAt = time.Time{}
	i.touchLocked()
	return true
}

// ConsumeTaskRunInterrupted reports once that a restore's ConfirmLive ended an
// in-flight task run as interrupted (#4222). The daemon calls it at its restore
// sites to stamp the owning task's run status; in-memory and one-shot, so a
// restart before the consume keeps the prior status — matching every other
// best-effort status write.
func (i *Instance) ConsumeTaskRunInterrupted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	interrupted := i.taskRunInterruptedStamp
	i.taskRunInterruptedStamp = false
	return interrupted
}

// TaskRunFailed reports whether this session's task run ended failed — a
// proven-undelivered initial delivery (#5278) — rather than on an agent-idle
// completion edge. Sticky for the process's lifetime; the load path re-derives
// it from the persisted verdict. Used by the daemon's completion predicate to
// tell the ways the run marker clears apart.
func (i *Instance) TaskRunFailed() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.taskRunFailed
}

// ConsumeTaskRunFailed reports once that a task run just ended failed (#5278).
// The daemon consumes it at its run-end observation sites to stamp the owning
// task's run status; one-shot per process, and the load path re-arms it from
// the same persisted verdict, so the stamp is idempotent across restarts.
func (i *Instance) ConsumeTaskRunFailed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	failed := i.taskRunFailedStamp
	i.taskRunFailedStamp = false
	return failed
}

// AckTaskRunFailureStamped clears the durable pending-failure flag once the
// daemon has settled the owning task's status write (applied, or refused as
// belonging to another run) so a restart does not re-drive it (#5646).
func (i *Instance) AckTaskRunFailureStamped() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.taskRunFailurePending {
		i.taskRunFailurePending = false
		// Mark the row dirty so the cleared flag reaches disk with the next
		// checkpoint (#5662). The task's run-keyed status write is idempotent
		// against a stale re-arm either way, so this is belt and braces.
		i.touchLocked()
	}
}

// ReconcileIdleEvidence mirrors the daemon's evidence onto a client row model.
// It applies both directions because runtime replacement can clear or replace
// the evidence, and the daemon snapshot is authoritative for all five fields.
func (i *Instance) ReconcileIdleEvidence(attemptedAt time.Time, status PromptDeliveryStatus, churnAt,
	taskAttemptAt, turnAt time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastPromptAttemptAt.Equal(attemptedAt) &&
		i.lastPromptDeliveryStatus == status &&
		i.lastPaneChurnAt.Equal(churnAt) &&
		i.taskRunPromptAttemptAt.Equal(taskAttemptAt) &&
		i.taskRunTurnObservedAt.Equal(turnAt) {
		return false
	}
	i.lastPromptAttemptAt = attemptedAt
	i.lastPromptDeliveryStatus = status
	i.lastPaneChurnAt = churnAt
	i.taskRunPromptAttemptAt = taskAttemptAt
	i.taskRunTurnObservedAt = turnAt
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
	if !runGateActiveInData(data) {
		return time.Time{}
	}
	return data.TaskRunTurnObservedAt
}

// taskRunPromptAttemptAtFromData is the same restore-side gate for the run's
// own prompt boundary: evidence written for a finished run carries no window.
// A row persisted by a release that predates the task-scoped fields keeps an
// active run with only the session-level send on record — that send IS the
// task's prompt for an upgrade-era row, so adopt it rather than restart with
// an unarmed gate that lets the first Ready tick complete the run (#5221).
// The adoption skips a proven-failed send: RecordPromptAttempt never binds a
// PromptNotDelivered attempt to the boundary while the daemon is up, so a
// restart must not resurrect one — the same Ready tick that would end the
// un-restarted run must end this one too, not wedge it until fallback.
func taskRunPromptAttemptAtFromData(data InstanceData) time.Time {
	if !runGateActiveInData(data) {
		return time.Time{}
	}
	if data.TaskRunPromptAttemptAt.IsZero() {
		if data.LastPromptDeliveryStatus == PromptNotDelivered {
			return time.Time{}
		}
		return data.LastPromptAttemptAt
	}
	return data.TaskRunPromptAttemptAt
}

// IdleReasonSnapshot returns the derived reason and last observed pane churn in
// one lock hold for row renderers.
func (i *Instance) IdleReasonSnapshot() (IdleReason, time.Time) {
	reason, _, _, churnAt := i.IdleReasonDetailSnapshot()
	return reason, churnAt
}

// IdleReasonDetailSnapshot adds the structured restore outcome needed by row
// renderers — the terminal failure and the environmental wait — under the same
// lock as the derived reason.
func (i *Instance) IdleReasonDetailSnapshot() (IdleReason, *LostRestoreFailure, *LostRestoreWait, time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	data := InstanceData{
		Status:                   i.statusLocked(),
		Liveness:                 i.liveness,
		InFlightOp:               i.inFlightOp,
		UserKilled:               i.userKilled,
		LostRestoreFailure:       cloneLostRestoreFailure(i.lostRestoreFailure),
		LostRestoreWait:          cloneLostRestoreWait(i.lostRestoreWait),
		RootRecreateContext:      i.rootRecreateContext,
		LastPromptAttemptAt:      i.lastPromptAttemptAt,
		LastPromptDeliveryStatus: i.lastPromptDeliveryStatus,
		LastPaneChurnAt:          i.lastPaneChurnAt,
	}
	return IdleReasonFor(data), cloneLostRestoreFailure(i.lostRestoreFailure),
		cloneLostRestoreWait(i.lostRestoreWait), i.lastPaneChurnAt
}
