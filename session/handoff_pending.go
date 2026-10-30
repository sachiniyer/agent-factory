package session

import "fmt"

// SetPendingHandoffMission records the rendered takeover brief before the
// irreversible runtime-swap checkpoint. A daemon restart can then recover the
// exact context that still needs delivery instead of guessing from Prompt.
func (i *Instance) SetPendingHandoffMission(mission string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission {
		i.pendingHandoffMission = mission
		// Recording the obligation precedes submission, so this is positive
		// mission-scoped evidence that an automatic attempt is initially safe.
		i.handoffDeliveryStatus = PromptNotDelivered
		i.touchLocked()
	}
}

// PendingHandoffMission returns the takeover brief awaiting confirmed delivery.
func (i *Instance) PendingHandoffMission() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission
}

// BeginPendingHandoffMissionDelivery fails closed before submission. The caller
// persists this marker before touching the composer, closing the crash window in
// which an attempt may land without its verdict becoming durable.
func (i *Instance) BeginPendingHandoffMissionDelivery(mission string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission || mission == "" {
		return fmt.Errorf("pending handoff mission changed before delivery")
	}
	if i.handoffDeliveryStatus != PromptCouldNotConfirm {
		i.handoffDeliveryStatus = PromptCouldNotConfirm
		i.touchLocked()
	}
	return nil
}

// RecordPendingHandoffMissionDelivery binds the runtime verdict to this exact
// mission. Empty and future verdicts are ambiguity, never retry authorization.
func (i *Instance) RecordPendingHandoffMissionDelivery(mission string, status PromptDeliveryStatus) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission || mission == "" {
		return fmt.Errorf("pending handoff mission changed during delivery")
	}
	if !status.Valid() {
		status = PromptCouldNotConfirm
	}
	if i.handoffDeliveryStatus != status {
		i.handoffDeliveryStatus = status
		i.touchLocked()
	}
	return nil
}

// PendingHandoffDeliveryStatus returns the verdict recorded for the pending
// handoff mission, or "" when no mission is pending. A caller that raises the
// attempt marker reads it first, so a failed marker write can put back the
// verdict it replaced instead of assuming one.
func (i *Instance) PendingHandoffDeliveryStatus() PromptDeliveryStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.handoffDeliveryStatus
}

// PendingHandoffMissionAutoRetryable permits automatic redelivery only after a
// mission-scoped observation proved that the exact pending mission did not land.
func (i *Instance) PendingHandoffMissionAutoRetryable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission != "" && i.handoffDeliveryStatus == PromptNotDelivered
}

// pendingHandoffMissionNeedsFence reports whether a durable pending mission
// must reconstruct the OpReplacing fence on load (#4429). The fence protects
// the two verdicts that still own an in-flight obligation the daemon resolves
// itself — positive non-delivery (automatic replay owns the resend; the row
// stays inert until it lands) and the delivered crash window (the recovery
// settle owns the bookkeeping). Ambiguous verdicts deliberately load WITHOUT
// the fence: the remaining confirm-or-retry decision is the operator's, both
// exits prove the runtime before acting, and rebuilding the fence there
// manufactures the wedge. FromInstanceData spells out how an ambiguous verdict
// can become durable.
func pendingHandoffMissionNeedsFence(status PromptDeliveryStatus) bool {
	return status == PromptNotDelivered || status == PromptDelivered
}

// PendingHandoffMissionSettleable reports whether the pending mission's
// recorded verdict already proves delivery, so recovery can retire it without
// a resend or an operator attestation — the crash window between a delivered
// record and its clearing settle.
func (i *Instance) PendingHandoffMissionSettleable() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingHandoffMission != "" && i.handoffDeliveryStatus == PromptDelivered &&
		!i.userKilled
}

// CanRetryPendingHandoffMissionDelivery reports whether an operator can inspect
// the known incoming pane and explicitly override an ambiguous mission verdict.
// Positive non-delivery belongs to automatic recovery; delivered evidence and
// an unknown/missing runtime never authorize another submission.
//
// A startup-unknown row DOES admit the explicit retry (#4429). Its `started`
// bit is down, and every local send and pane capture refuses a row in that
// state, so the daemon first probes the pane under the op lock: only a runtime
// that answers restores `started` (ResolveStartupState) before the readiness
// wait and the send run. A probe that does not answer refuses the retry and
// leaves the row as it was.
func (i *Instance) CanRetryPendingHandoffMissionDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	ambiguous := ambiguousHandoffDelivery(i.handoffDeliveryStatus)
	// Dead rows keep their restore/kill handles; neither a resend nor an
	// attestation is meaningful against a runtime that no longer exists.
	dead := i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived
	return i.pendingHandoffMission != "" && ambiguous && !i.userKilled && !dead &&
		(i.inFlightOp == OpNone || i.inFlightOp == OpReplacing) &&
		(i.liveness == LiveRunning || i.liveness == LiveReady || i.startupStateUnknown)
}

// CanConfirmPendingHandoffDelivery reports whether an operator can retire the
// pending mission without resending it — the "it already landed" exit (#4429).
// The verdict must already be recorded and ambiguous-or-positive: an
// unrecorded or positively-absent delivery belongs to the send path, and
// not-delivered belongs to automatic recovery. Startup-unknown rows are the
// ones this verb exists FOR, so the flag is not a refusal here — the daemon
// probes the pane before honoring the attestation.
func (i *Instance) CanConfirmPendingHandoffDelivery() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingHandoffMission == "" || i.userKilled ||
		(i.inFlightOp != OpNone && i.inFlightOp != OpReplacing) ||
		!confirmableHandoffDelivery(i.handoffDeliveryStatus) {
		return false
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return false
	}
	return i.startupStateUnknown ||
		i.liveness == LiveRunning || i.liveness == LiveReady || i.liveness == LiveLimitReached
}

// ConfirmPendingHandoffDelivery retires the pending handoff mission on the
// operator's attestation that it already landed (#4429): no resend, no new
// observation — the pane inspection happened at the terminal, not here. The
// daemon probes the runtime before calling; this method re-checks only the
// durable facts the attestation discharges.
//
// It resolves the whole wedge in one critical section: an OpReplacing fence
// settles through the CommitHandoff edge (the runtime was proven to reach this
// point — it accepted the paste), a startup-unknown flag lifts with started
// restored (the probe that admitted this call is the identity proof that flag
// was waiting for), and the mission plus its verdict clear together so no
// later reader reconstructs the fence. Refusing not-delivered keeps automatic
// recovery's ownership unambiguous.
//
// The attestation also decides the row's liveness (#5023): a mission the
// incoming agent already received means the agent HAS work, so the confirmed
// row reads LiveRunning — the same state a delivered prompt produces — until
// the status monitor observes a genuinely idle pane. A fenced row gets that
// from CommitHandoff already; the explicit edge below is what an unfenced
// ambiguous row — the could-not-confirm settle — needs to not publish as a
// settled idle session.
//
// A row parked at its usage-limit wall keeps LiveLimitReached instead — the
// parked state is still honest (the mission cannot run until quota resets),
// still not-idle to fleet watch, and is the marker ResumeLimitedSessions scans
// for. The fenced arm parks through ParkHandoff, the same edge a live handoff
// that hit the wall takes, so the fence drops without CommitHandoff publishing
// LiveRunning and discarding the reset bookkeeping.
func (i *Instance) ConfirmPendingHandoffDelivery(mission string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission == "" || i.pendingHandoffMission != mission {
		return fmt.Errorf("session %q has no pending handoff mission matching this confirmation", i.Title)
	}
	if i.userKilled {
		return fmt.Errorf("session %q has a pending kill", i.Title)
	}
	if i.liveness == LiveLost || i.liveness == LiveDead || i.liveness == LiveArchived {
		return fmt.Errorf("session %q has no live runtime to confirm against (liveness %v); restore owns this row", i.Title, i.liveness)
	}
	if !confirmableHandoffDelivery(i.handoffDeliveryStatus) {
		return fmt.Errorf("session %q has no ambiguous handoff delivery to confirm (status %q); automatic recovery owns not-delivered missions", i.Title, i.handoffDeliveryStatus)
	}
	if i.inFlightOp != OpNone && i.inFlightOp != OpReplacing {
		return fmt.Errorf("session %q is busy (%v)", i.Title, i.inFlightOp)
	}
	lv, op, resetAt := i.lifecycleStateLocked()
	i.resolveStartupStateLocked()
	if i.inFlightOp == OpReplacing {
		ev := CommitHandoff()
		if i.liveness == LiveLimitReached {
			// The incoming agent is parked at its usage-limit wall: settle the
			// fence the way a handoff that hit the wall live does, so the row
			// stays inside ResumeLimitedSessions' scan. CommitHandoff would
			// publish LiveRunning and drop the reset bookkeeping outright.
			ev = ParkHandoff(resetAt)
		}
		if err := i.transitionLocked(ev); err != nil {
			return err
		}
	}
	i.pendingHandoffMission = ""
	i.handoffDeliveryStatus = ""
	// A confirmed mission is work the incoming agent already has (#5023) —
	// publish working (a no-op on the CommitHandoff arm, which already landed
	// there) and leave the settle back to Ready to the monitor's pane
	// evidence. A limit-blocked row keeps its wall: the parked row still reads
	// not-idle to fleet watch, and a paused poll cannot re-park a row this
	// edge would have falsely marked running.
	if i.liveness != LiveLimitReached {
		_ = i.transitionLocked(ObserveLiveness(LiveRunning))
	}
	i.touchLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
	return nil
}

// resolveStartupStateLocked clears the startup-unknown fence once a fresh proof
// — a live-pane probe or an operator attestation accepted under it — has
// re-established the runtime binding. MarkStartupStateUnknown lifted `started`
// to keep attach/probe paths from trusting the unconfirmed name; restoring it
// here is the other half of the same fact. Caller holds i.mu. The task-run
// marker is deliberately untouched: runs only ever go true→false.
func (i *Instance) resolveStartupStateLocked() {
	if i.startupStateUnknown {
		i.startupStateUnknown = false
		i.touchLocked()
	}
	if !i.started {
		i.started = true
		i.touchLocked()
	}
}

// ResolveStartupState is the locking form of resolveStartupStateLocked. The
// daemon calls it once a liveness probe has answered: before an explicit retry
// sends to a startup-unknown row, and when recovery settles a delivered
// mission on a live row.
func (i *Instance) ResolveStartupState() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	i.resolveStartupStateLocked()
	i.noteStateChangeLocked(lv, op, resetAt)
}

// ReconcilePendingHandoffSnapshot mirrors the daemon-owned agent handoff
// obligation onto an existing client projection. Open TUIs update rows in place,
// so copying only OpReplacing would leave the explicit retry predicate blind to
// the mission and its mission-scoped verdict until the client restarted.
func (i *Instance) ReconcilePendingHandoffSnapshot(mission string, status PromptDeliveryStatus) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission == mission && i.handoffDeliveryStatus == status {
		return false
	}
	i.pendingHandoffMission = mission
	i.handoffDeliveryStatus = status
	i.touchLocked()
	return true
}

// ClearPendingHandoffMission clears the marker only if it still names mission.
// The compare makes a delayed recovery attempt unable to erase a newer handoff's
// brief after the same session has moved on.
//
// Its callers have just sent the mission, or handed it to the limit resume that
// will, so the agent is about to work and its run ends on that work's own idle
// edge. The held idle edge is dropped in the same critical section: left set, an
// idle tick that lands before the agent picks the turn up would end the run —
// and hand a working session to its task's on_complete policy. Retiring a
// mission WITHOUT a resend is ConfirmPendingHandoffDelivery, which keeps it.
func (i *Instance) ClearPendingHandoffMission(mission string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingHandoffMission != mission {
		return false
	}
	if i.pendingHandoffMission != "" {
		i.pendingHandoffMission = ""
		i.handoffDeliveryStatus = ""
		i.taskRunIdleEdgeHeld = false
		i.taskRunTurnGateHeld = false
		i.touchLocked()
	}
	return true
}

// MarkStartupStateUnknown retains an uncertain create or restore as an inert
// record. Clearing started prevents attach/probe paths from treating the
// requested runtime name as confirmed; StartupStateUnknown keeps storage
// checkpoints from dropping the record merely because it is not started.
func (i *Instance) MarkStartupStateUnknown() {
	i.mu.Lock()
	defer i.mu.Unlock()
	lv, op, resetAt := i.lifecycleStateLocked()
	if !i.startupStateUnknown {
		// RuntimeProgram stops being proof of a current runtime at this edge.
		// Advance the lock-free evidence generation before publishing the fence so
		// an asynchronous drift check cannot latch the previously known command.
		i.runtimeEvidenceGeneration.Add(1)
		i.startupStateUnknown = true
		i.touchLocked()
	}
	if i.started {
		i.started = false
		i.touchLocked()
	}
	// Startup-unknown is a terminal delivery outcome, not a run still consuming
	// the task's concurrency budget. Store that fact on the same transition that
	// stores the terminal marker so projections, persistence, and unloadable-row
	// accounting cannot disagree about whether the slot was released.
	// Same section, same reason as the completion transition (#3865): this is the
	// other edge that clears the run marker, so it is the other place the adoption
	// baseline has to be pinned. On the EDGE, not beside the assignment below,
	// which is unconditional — a second call once the run has already ended must
	// not pin a fresh baseline over a teardown's, since that would fold a delivery
	// made in between into the baseline and read as though nothing had happened.
	if i.taskRunActive {
		i.captureAdoptionBaselineLocked()
		i.taskRunActive = false
		i.touchLocked()
	}
	if i.backlogRunActive {
		i.backlogRunActive = false
		i.touchLocked()
	}
	// The create attempt has settled into an explicit blocked outcome. Leaving
	// OpCreating set makes projections report an operation that no goroutine owns
	// and can keep old clients polling forever.
	i.inFlightOp = OpNone
	i.noteStateChangeLocked(lv, op, resetAt)
}

// StartupStateUnknown reports whether a create may have launched a runtime but
// could not confirm its identity or liveness.
func (i *Instance) StartupStateUnknown() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.startupStateUnknown
}
