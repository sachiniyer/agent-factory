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
	// Churn within a quiet grace of this send is possibly an in-flight turn —
	// evaluated BEFORE the fence below can clear the stamp, so a capture that
	// raced the send counts as activity at the attempt, not as a clean pane.
	churnNearAttempt := !i.lastPaneChurnAt.IsZero() &&
		attemptedAt.Sub(i.lastPaneChurnAt) < taskRunCompletionQuietGrace
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
	if i.taskRunActive && status != PromptNotDelivered &&
		(i.taskRunPromptAttemptAt.IsZero() ||
			!i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt)) {
		rebind := !i.taskRunPromptAttemptAt.IsZero()
		i.taskRunPromptAttemptAt = attemptedAt
		// The new boundary re-derives its stillness edge from scratch — a
		// candidate open under the old attempt proves nothing past the new
		// send (#5221 review).
		i.taskRunStillCandidateAt = time.Time{}
		// Post-boundary churn may stand in for the turn only if the pane was
		// observed still at-or-after the attempt (#5221 review). A first bind
		// seeds the base at the send — boot/echo churn is the very window the
		// quiet arm exists to absorb. A rebind seeds only when the pane had
		// already gone still for a full grace: a redelivery landing on a
		// churning pane may be mid-turn, so its churn must wait for the first
		// confirmed-unchanged capture to be attributable.
		if !rebind || !churnNearAttempt {
			i.taskRunStillBaseAt = attemptedAt
		} else {
			i.taskRunStillBaseAt = time.Time{}
		}
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
// churn after the latest prompt — and the first after a latched stillness
// base. Both edges must be persisted even while the row remains Running:
// the prompt edge marks answer-start evidence, and the base edge is the
// attributable churn the quiet arm reads after a restore — a restart that
// kept only pre-base churn would see a pane that never moved post-base and
// demote the release from the quiet arm to the five-minute silent arm
// (#5221 review). Later spinner churn needs no write on every poll.
func (i *Instance) RecordPaneChurnCheckpointAtEpoch(churnAt time.Time, observedEpoch uint64) (bool, bool) {
	if churnAt.IsZero() {
		return false, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch || !churnAt.After(i.lastPaneChurnAt) {
		return false, false
	}
	checkpoint := (!i.lastPromptAttemptAt.IsZero() &&
		churnAt.After(i.lastPromptAttemptAt) &&
		!i.lastPaneChurnAt.After(i.lastPromptAttemptAt)) ||
		(!i.taskRunStillBaseAt.IsZero() &&
			churnAt.After(i.taskRunStillBaseAt) &&
			!i.lastPaneChurnAt.After(i.taskRunStillBaseAt))
	i.lastPaneChurnAt = churnAt
	// An updated frame ends the still run — a candidate mid-grace was not
	// the turn's end, so the owed edge re-derives from the next unchanged
	// stretch (#5221 review).
	i.taskRunStillCandidateAt = time.Time{}
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

// taskRunStillGrace is how long a post-attempt pane must sit continuously
// unchanged before that stillness counts as the owed edge: a single static
// frame — a paused spinner, an idle prompt redraw — is one sample, not proof
// the prior turn ended (#5221 review). The floor matches the turn watcher's
// absence grace: the same "transient vs sustained" cut on the pane side. A
// var so tests can compress it.
var taskRunStillGrace = 5 * time.Second

// NotePaneStillAtEpoch latches the stillness edge a contended boundary owes
// (#5221 review): confirmed-UNCHANGED captures at-or-after the run's prompt
// attempt, sustained for taskRunStillGrace. A boundary bound while the pane
// was already churning — a redelivery landing mid-turn — cannot attribute
// churn that began before the send, so only churn following this edge may
// stand in for the turn. The edge latches at the START of the sustained still
// run: one unchanged sample may be a static thinking frame, so the first
// capture only opens the candidate and churn between samples retires it —
// stillness that did not survive taskRunStillGrace never proved the old turn
// ended. A still capture predating the attempt proves nothing about the
// activity it interrupted. Epoch-fenced like RecordPaneChurnCheckpointAtEpoch.
func (i *Instance) NotePaneStillAtEpoch(at time.Time, observedEpoch uint64) bool {
	if at.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch || i.taskRunPromptAttemptAt.IsZero() ||
		at.Before(i.taskRunPromptAttemptAt) || !i.taskRunStillBaseAt.IsZero() {
		return false
	}
	if i.taskRunStillCandidateAt.IsZero() {
		i.taskRunStillCandidateAt = at
		return false
	}
	if at.Sub(i.taskRunStillCandidateAt) < taskRunStillGrace {
		return false
	}
	i.taskRunStillBaseAt = i.taskRunStillCandidateAt
	i.touchLocked()
	return true
}

// NotePaneWaitingAtEpoch retires an open stillness candidate on affirmative
// evidence the pane is mid-turn awaiting input (#5221 review): a
// confirmation dialog is an active waiting state, not stillness, so a still
// run interrupted by one must re-earn the whole grace prompt-free. The
// already-latched base is untouched — it records a still edge the pane
// genuinely showed earlier, and a later dialog cannot un-prove that past.
// Epoch-fenced like NotePaneStillAtEpoch.
func (i *Instance) NotePaneWaitingAtEpoch(observedEpoch uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch {
		return
	}
	i.taskRunStillCandidateAt = time.Time{}
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
	// The churn must be ATTRIBUTABLE to this boundary — begun after the pane
	// was observed still at-or-after the attempt. A bound-over-activity
	// boundary leaves taskRunStillBaseAt owed until the first confirmed-still
	// capture, and churn continuous since before the send never qualifies
	// (#5221 review).
	if i.taskRunStillBaseAt.IsZero() || !i.lastPaneChurnAt.After(i.taskRunStillBaseAt) {
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
	// The pane must have stayed silent since the later of the attempt and the
	// stillness base: for a boundary bound over in-flight activity, the base
	// is the first still frame AFTER that activity ended, and the churn that
	// preceded it belongs to the old work — not a disqualifier, but never the
	// silence this arm measures either. ATTRIBUTABLE churn (after the base)
	// means the pane IS answering — that run belongs to the quiet arm, which
	// demands its own 30s of silence after the last output (#5221 review).
	base := i.taskRunPromptAttemptAt
	if i.taskRunStillBaseAt.After(base) {
		base = i.taskRunStillBaseAt
	}
	if i.lastPaneChurnAt.After(base) {
		return false
	}
	if !i.paneObservationLiveLocked() {
		return false
	}
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
	// The observation streak describes the retired pane's captures: a
	// replacement owns none of them, and at slow cadences its unexpired
	// horizon would let the new pane's first frame count watch time nobody
	// spent on it — the silent arm could then satisfy its grace on wall
	// clock that ran during the swap (#5221 review).
	if !i.lastPaneCaptureAt.IsZero() || !i.paneObservationStreakAt.IsZero() ||
		!i.paneObservationExpiresAt.IsZero() {
		i.lastPaneCaptureAt = time.Time{}
		i.paneObservationStreakAt = time.Time{}
		i.paneObservationExpiresAt = time.Time{}
		changed = true
	}
	// The task-run boundary and turn observation are pane-relative evidence of
	// the RETIRED runtime: a replacement pane owns neither the send it did not
	// make nor the chrome it did not show, so the delivery window does not
	// carry across the swap — the run's remaining life follows the pane the
	// same way it did before the gate existed (#5221 review).
	if !i.taskRunPromptAttemptAt.IsZero() || !i.taskRunTurnObservedAt.IsZero() {
		i.taskRunPromptAttemptAt = time.Time{}
		i.taskRunTurnObservedAt = time.Time{}
		i.taskRunStillBaseAt = time.Time{}
		i.taskRunStillCandidateAt = time.Time{}
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
	if !data.TaskRunActive {
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
	if !data.TaskRunActive {
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

// taskRunStillBaseAtFromData restores the stillness edge the quiet arm's
// churn must follow (#5221 review). A persisted value survives verbatim; a
// zero one is re-derived exactly as the bind decides it — churn within the
// quiet grace of the attempt is possibly an in-flight turn, so the edge
// stays owed, while anything staler seeded the base at the send. Rows that
// predate the field re-derive the same answer rather than inheriting a
// blanket seed.
func taskRunStillBaseAtFromData(data InstanceData, taskRunAttemptAt time.Time) time.Time {
	if !data.TaskRunActive || taskRunAttemptAt.IsZero() {
		return time.Time{}
	}
	if !data.TaskRunStillBaseAt.IsZero() {
		return data.TaskRunStillBaseAt
	}
	if data.LastPaneChurnAt.IsZero() ||
		taskRunAttemptAt.Sub(data.LastPaneChurnAt) >= taskRunCompletionQuietGrace {
		return taskRunAttemptAt
	}
	return time.Time{}
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
