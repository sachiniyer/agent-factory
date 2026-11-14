package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// The lifecycle transition chokepoint (#1195 Phase 2c).
//
// Phase 1 split the session state into two axes — liveness (daemon-owned health)
// and inFlightOp (client-owned operation) — but the ~15 writers still poke those
// fields through a scatter of setters (SetLiveness, SetInFlightOp, MarkLive,
// SetArchived, RestoreFromArchive, …) with NO transition table: the ordering
// invariants that keep the lifecycle sound are enforced only by convention at
// the call sites. Transition makes them structural — a single validated
// chokepoint every writer routes through — so an illegal or mis-ordered
// transition is caught by the edge table instead of silently corrupting state.
//
// The four ordering invariants it enforces (audit item #6):
//   - I1 tombstone-before-teardown is NOT a (liveness,op) edge, so it is NOT
//     enforced here: the tombstone (userKilled) is a daemon-side field the
//     client optimistic kill overlay never has, and daemon-internal kills (reap,
//     create-cleanup) legitimately carry no tombstone. The daemon KillSession
//     enforces I1 by ordering (MarkUserKilled before Instance.Kill) while setting
//     NO op — keeping the snapshot pure liveness for out-of-band kills. BeginKill
//     is therefore an unconstrained optimistic overlay. I2–I4 below are genuine
//     edges the table enforces.
//   - I2 move-before-Archived: the inert Archived state is reachable ONLY out of
//     the archive fence, which is only entered after the worktree move ran.
//   - I3 move-before-respawn: a restore may begin only from Archived, and the
//     daemon moves the worktree back before entering the restore fence.
//   - I4 fence-brackets-archive: Archived/Lost is reachable from an archive only
//     through the OpArchiving fence, so the poll/tab-spawn skip the whole window.
//
// Phase 2c lands this INERT: the API + table + tests, with no call-site
// migration. Phase 2d migrates the daemon/app writers onto it and deletes the
// ad-hoc guards; Phase 2e retires the direct setters.

// onIllegalTransition, when non-nil, is invoked with the rejection message
// before Transition returns the soft error. Test builds install a panicking hook
// (an illegal, mis-ordered transition must be a loud red failure, never a silent
// prod corruption); production leaves it nil and degrades to the soft error — a
// user's daemon must not crash on a racing double-archive. Keeps `testing` out
// of the production binary.
var onIllegalTransition func(msg string)

// SetIllegalTransitionHook installs fn as the illegal-transition hook and returns
// a restore func. It exists so test binaries in OTHER packages (app, daemon) can
// install the same panic-on-illegal guard the session tests install directly — a
// mis-ordered transition must be a loud failure everywhere a writer routes
// through the chokepoint, not only in session-package tests. Production never
// calls this: the hook stays nil and an illegal edge degrades to the soft error.
func SetIllegalTransitionHook(fn func(msg string)) (restore func()) {
	prev := onIllegalTransition
	onIllegalTransition = fn
	return func() { onIllegalTransition = prev }
}

// Transition applies a lifecycle event to the two-axis (liveness, inFlightOp)
// state under i.mu, validating it against the allowed-edge table. It is the
// single writer-side chokepoint for lifecycle-state changes (#1195 Phase 2c) and
// the enforcement point for the I1–I4 ordering invariants. An illegal edge
// returns an error AND fires onIllegalTransition (panic in test builds); a
// yielding edge (ObserveLiveness always, ConfirmLive under a teardown op) that
// is out-of-set is a silent no-op. INERT until Phase 2d migrates the writers.
func (i *Instance) Transition(ev TransitionEvent) error {
	if ev.kind == tkConfirmLive {
		i.runLiveBoundary()
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.transitionLocked(ev)
}

// transitionLocked is Transition's already-locked half. Caller holds i.mu for
// writing. It exists so a chokepoint that must validate and mutate in ONE critical
// section can do both without releasing the lock between them — see
// lifecycleViewLocked, and BeginLimitResume for a case where the gap was a real
// race (#2997).
func (i *Instance) transitionLocked(ev TransitionEvent) error {
	if ev.epochScoped && i.stateEpoch != ev.epoch {
		// The decision behind this event was drawn from an observation that a newer
		// authoritative transition has since superseded. Drop it — silently and
		// without an error, since being out-competed by newer truth is not a
		// mis-ordered edge (#2135). The observer re-decides on its next tick.
		return nil
	}
	if ev.kind == tkObserveLiveness && ev.churnOnly && ev.lv == LiveRunning {
		keep := cloneAttention(i.attention)
		defer func() {
			if keep != nil && i.liveness == LiveRunning {
				i.attention = keep
			}
		}()
	}
	if ev.kind == tkBeginArchive && i.archiveCheckpointSealed {
		return fmt.Errorf("session %q cannot begin an archive after the daemon shutdown checkpoint was sealed", i.Title)
	}
	if ev.kind == tkBeginArchive && i.pendingAccountSwap != nil {
		return fmt.Errorf("session %q has %s awaiting its replacement notice and task; retry it with %s before archiving",
			i.Title, accountSwapSpan(i.pendingAccountSwap.From, i.pendingAccountSwap.To), accountSwapRetryCommand(i.Title, i.pendingAccountSwap.To))
	}
	if ev.kind == tkObserveLiveness && ev.lv != LiveLost && i.rebuildTeardownPending {
		// A committed worktree rebuild whose stale-pane teardown has not been
		// confirmed means a live answer can still come from a pane bound to the
		// DELETED inode — the observation cannot distinguish that survivor from
		// a fresh pane, so it must not lift the row out of Lost. Leaving it Lost
		// is what keeps the restore loop retrying the teardown instead of
		// exposing the stale-cwd pane as healthy again (#5371 review). The flag
		// is cleared only after every bound name is confirmed gone, so a
		// positive claim before then is held as Lost; a LiveLost observation
		// passes through untouched.
		ev.lv = LiveLost
	}
	from := stateAxes{i.liveness, i.inFlightOp}
	spec, ok := transitionTable[ev.kind]
	if !ok {
		return i.rejectTransitionLocked(ev, from, "unknown event")
	}
	if !spec.allowedFrom(from) {
		if spec.yieldWhenBlocked {
			return nil
		}
		return i.rejectTransitionLocked(ev, from, "edge not allowed from this state")
	}

	to := spec.target(from, ev)
	if to != from {
		// Invalidate async runtime evidence before publishing a replacement fence
		// or a liveness change. noteStateChangeLocked advances it again after the
		// full mutation; the two increments deliberately bracket the write so a
		// lock-free validator cannot accept a half-applied transition.
		i.runtimeEvidenceGeneration.Add(1)
	}
	if ev.kind == tkBeginArchive {
		i.archiveSettled = make(chan struct{})
	}
	// The durable-push fence belongs to exactly one archive generation. Begin
	// clears any historical value before the remote push starts; every settling
	// edge clears it after checkpoint retention no longer needs the phase.
	switch ev.kind {
	case tkBeginArchive, tkCancelArchive, tkCommitArchive, tkAbortArchiveToLost:
		i.archivePushCompleted = false
	}
	// Apply this transition's declared effect on the task run (#1892). The answer
	// comes from the table, not from reading the resulting state — see runEffect.
	//
	// Only ever true→false: a capped task creates one session per event, so a
	// session has exactly one run, and anything that happens to it afterwards
	// (a user prompting it, an un-archive) is not the task's.
	//
	// endRunsLocked clears the marker and the held edges. The replaced-runtime
	// interruption below is the TASK run's contract (#4222).
	if i.runGateActiveLocked() {
		if i.taskRunActive && ev.kind == tkConfirmLive && from.op == OpRestoring && i.agentRuntimeReplaced {
			// A restore landing on a REPLACED runtime ends a still-in-flight
			// run as interrupted (#4222): the prompt's owner died with the old
			// process and the replacement is never replayed the prompt, so the
			// idle edge it is about to produce is a fresh agent at rest — not a
			// finished run. Ending it here, inside the edge that drops the
			// restore fence, keeps the marker away from the first idle
			// ObserveLiveness, which is where the run used to "complete" and
			// hand on_complete a session whose interrupted work it would
			// destroy. A verified reattach takes the normal path: the recorded
			// agent still owns the prompt, so its next idle edge remains a
			// genuine completion.
			i.endTaskRunInterruptedLocked()
			i.taskRunInterruptedStamp = true
		} else {
			switch spec.run {
			case runEnds:
				// The completion transition IS the capture point for the adoption
				// baseline (#3865): taken here, inside the same i.mu section that ends
				// the run, nothing — not the rest of this transition, not the poll's
				// later persistPollChange — can land a delivery that reads as though it
				// had always been there. See session/adoption_fence.go.
				i.endRunsLocked()
			case runEndsOnIdleEdge:
				// The AGENT's own axis, and the EDGE into it. Not ClassifyActivity — that
				// calls an in-flight archive "pending", which would miss an agent going
				// idle mid-teardown. Not the resulting state alone — a session is born
				// LiveReady before its agent ever runs, so that would end the run at birth.
				//
				// An idle pane is not a finished run while a handoff mission is still
				// owed (#4429). The incoming agent may be idle precisely BECAUSE its
				// takeover brief never landed, and this edge is what hands a task
				// session to its on_complete policy — so ending the run here is what
				// would archive or kill a session whose mission is still waiting for
				// the operator. The replacement fence used to hide the row from the
				// poll and carry this; the fence now settles on the incoming runtime's
				// liveness, so the obligation itself has to say so.
				//
				// A held edge is recorded rather than dropped. A resend makes the
				// agent work, and Mark delivered now likewise leaves the ROW working
				// — the confirm publishes LiveRunning, or keeps a LiveLimitReached
				// wall (#5023) — so the next idle observation lands as a real edge
				// into Ready and ends the run itself. The held marker still closes
				// the gap for an obligation retired while the row was already
				// recorded Ready — records written before #5023 carry exactly that —
				// whose later polls would otherwise see only Ready → Ready and hold
				// the run open forever.
				//
				// ANY idle observation while the mission is owed holds, not only a
				// transition into Ready. A row that reloads already Ready — every row
				// a pre-#4429 daemon fenced, on the upgrade that settles it — sees
				// only Ready → Ready, and would otherwise hold nothing and keep its
				// run open forever after the confirm. The edge rule exists so a
				// session born Ready does not end its run at birth; a row owing a
				// takeover mission is past birth by construction.
				//
				// An idle edge is likewise not a finished run while the run's own
				// prompt has been attempted but never demonstrably picked up (#5219).
				// ConfirmLive publishes LiveRunning the moment the prompt send
				// returns, so the FIRST quiet tick afterwards — long before an agent
				// that boots behind a still pane (devin's ACP startup takes seconds,
				// and no IsWorkingContent matcher covers it) has visibly reacted —
				// used to end the run and hand on_complete a session whose turn never
				// began. Pane churn alone is not the turn evidence: the same async
				// pipeline that hides boot also makes the prompt's own echo, ACP
				// init, and skill discovery land as ordinary Updated captures seconds
				// after Enter. What releases the gate is the agent's own in-turn
				// chrome — devin/claude's elapsed-timer row must tick, which boot
				// cannot fake — or a post-attempt burst followed by sustained quiet
				// past the completion grace (the arm that covers signature-less
				// agents and turns too fast for two captures). The edge holds rather
				// than drops: evidence landing while the row is already Ready (the
				// paused poll path folds churn into lastPaneChurnAt without a
				// liveness move) releases it through the flag exactly as the mission
				// hold does. A run that never produces it stays open — reported
				// in-flight, never a completion.
				if to.liveness == LiveReady {
					switch {
					case i.owesMissionDeliveryLocked():
						if !i.taskRunIdleEdgeHeld {
							i.taskRunIdleEdgeHeld = true
							i.touchLocked()
						}
					case i.taskRunAwaitingTurnLocked():
						if !i.taskRunTurnGateHeld {
							i.taskRunTurnGateHeld = true
							i.touchLocked()
						}
					case from.liveness != LiveReady || i.taskRunIdleEdgeHeld || i.taskRunTurnGateHeld:
						// The gate just opened. If no in-turn chrome was observed,
						// the quiet-fallback arm — churn plus sustained silence — is
						// what let this edge end the run: the weaker signal, so name
						// it in the log with the agent and the attempt's age (#5219).
						if !i.taskRunPromptAttemptAt.IsZero() &&
							!i.taskRunTurnObservedAt.After(i.taskRunPromptAttemptAt) &&
							(i.taskRunQuietReleaseLocked() || i.taskRunSilentReleaseLocked()) {
							log.InfoLog.Printf(
								"task run for session %q completed on the quiet fallback (no in-turn chrome observed): agent=%s elapsed_since_attempt=%s",
								i.Title, i.currentAgentNameLocked(),
								time.Since(i.taskRunPromptAttemptAt).Round(time.Second))
						}
						i.endRunsLocked()
					}
				}
			}
		}
	}
	prevResetAt := i.limitResetAt
	// Only an edge that MOVES liveness goes through the coerce-to-Lost guard. An
	// operation-only edge (ClearOp releasing a fence, BeginRespawn, …) leaves the
	// liveness it found, which may be a disposition the transaction owner
	// deliberately restored while a rebuild teardown is owed — re-assigning it
	// would coerce that back to Lost and strand the row (#5371 review).
	if to.liveness != from.liveness {
		i.assignLivenessLocked(to.liveness)
	}
	i.inFlightOp = to.op
	switch spec.limitReset {
	case limitResetClear:
		i.limitResetAt = time.Time{}
		i.limitResetEvidence = ""
		i.limitBannerPrefix = ""
		i.limitBannerCount = 0
		i.limitWallRuntimePID = 0
		i.limitWallRuntimeStartID = 0
		i.limitWallRuntimeGen = 0
		i.limitPromptSent = false
	case limitResetFromEvent:
		// The event's reset arrives with the phrase that produced it (the
		// handoff wait path resolved both), so the evidence slot records it —
		// keeping the daemon's first poll from re-parsing the unchanged banner
		// against a later clock (#6765). The banner anchor rides with it for
		// the same reason (#6823): the poll must be able to tell a clipped
		// re-read of THIS banner from a wall the pane shows afterward.
		// The sighting resolves against the wall the row already recorded
		// BEFORE the live fields take it: a re-read of the banner the row
		// parked on keeps the recorded claim's first parse instead of the
		// rolled-forward instant its re-parse minted (#6959). The event's
		// freshRuntime mark is the producer's knowledge that the sighting
		// came off a pane the row had never seen — new evidence no recorded
		// claim may veto.
		resetAt, evidence, promptSent := i.originalLimitResetLocked(i.currentAgentNameLocked(),
			ev.resetAt, ev.evidence, ev.banner, from.liveness == LiveLimitReached,
			ev.freshRuntime, time.Time{}, time.Time{})
		i.limitResetAt = resetAt
		i.limitResetEvidence = evidence
		i.limitBannerPrefix = ev.banner.Prefix
		i.limitBannerCount = ev.banner.Count
		i.limitPromptSent = promptSent
		// The wall was sighted on the runtime currently up (the incoming
		// one for a handoff park): pin the claim to it so a sighting after
		// a replacement cannot pass as its re-read (#6959 review).
		i.limitWallRuntimePID = i.runtimePID
		i.limitWallRuntimeStartID = i.runtimeStartID
		i.limitWallRuntimeGen = i.agentRuntimeGeneration
	}
	if ev.kind == tkParkHandoff {
		// The incoming runtime, not the retired predecessor, produced this wall.
		// Publish its identity and durable negative quota evidence in the same
		// critical section as LiveLimitReached so account-swap admission cannot
		// observe a limit without its provider/account attribution.
		i.attributeLimitIdentityLocked()
		i.noteLimitWallClaimLocked(i.currentAgentNameLocked(), i.limitResetAt, i.limitResetEvidence,
			LimitBanner{Prefix: i.limitBannerPrefix, Count: i.limitBannerCount})
		i.recordAccountLimitObservationLocked(i.currentAgentNameLocked(), i.Account, i.limitResetAt, i.limitResetEvidence, false)
	}
	// Every real change to the lifecycle state advances the epoch, so an observer
	// holding an older one learns its in-flight decision is stale (#2135).
	i.noteStateChangeLocked(from.liveness, from.op, prevResetAt)
	switch spec.started {
	case startedSet:
		if !i.started {
			i.started = true
			i.touchLocked()
		}
	case startedClear:
		if i.started {
			i.started = false
			i.touchLocked()
		}
	}
	// A model-change diagnostic belongs to one concrete agent process. Archive
	// retires that process, and restore starts a new one under the same Instance
	// identity; neither boundary may carry the predecessor's warning across.
	switch ev.kind {
	case tkCommitArchive, tkBeginRestore:
		i.clearAgentModelChangeLocked()
	}
	// An attention request belongs to the agent that wrote it: a committed
	// handoff swaps the runtime without crossing a liveness edge, so nothing
	// else drops the outgoing agent's question for the incoming one.
	if ev.kind == tkCommitHandoff {
		i.attention = nil
	}
	// tkCommitArchive is the single commit edge every archive route converges
	// on — local worktree relocation and remote branch-push alike — so it is the
	// one place the durable archive timestamp can be stamped without a second
	// per-route agreement (#5136). A re-archive overwrites it with the NEW
	// commit, which is correct: the age prune measures belongs to the shelf the
	// record currently sits on.
	if ev.kind == tkCommitArchive {
		i.archivedAt = time.Now()
	}
	// The mirror rule (#5136 review): a row that LEAVES the archive no longer
	// sits on the shelf its timestamp measures — restore moves LiveArchived to
	// Lost at the begin edge (both tkBeginRestore entries), so keying the
	// clear on the liveness boundary rather than one event kind also covers
	// any future un-archive edge. A later re-archive stamps the new shelf's
	// own commit above.
	if from.liveness == LiveArchived && to.liveness != LiveArchived {
		i.archivedAt = time.Time{}
		i.prune.nonce = ""
	}
	if from.op == OpArchiving && to.op != OpArchiving && i.archiveSettled != nil {
		close(i.archiveSettled)
		i.archiveSettled = nil
	}
	return nil
}

// endTaskRunInterruptedLocked ends an in-flight task run as interrupted: the
// runtime that received its prompt is gone and the replacement was never
// replayed it, so no later idle edge may report the run finished and hand
// on_complete a session whose work belongs to the interrupted run (#4222).
// Caller holds i.mu. The adoption baseline is pinned here for the same reason
// the other marker-clearing edges pin it: this is one of them.
func (i *Instance) endTaskRunInterruptedLocked() {
	i.taskRunActive = false
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	i.taskRunInterrupted = true
	i.taskRunInterruptedAt = time.Now()
	if at := i.agentReplacedAt; !at.IsZero() && at.Before(i.taskRunInterruptedAt) &&
		i.taskRunInterruptedAt.Sub(at) <= taskRunResumeWindow {
		i.taskRunInterruptedAt = at // the launch, not the settle (#6231)
	}
}

// endTaskRunFailedLocked ends an in-flight task run as failed: the run's own
// prompt was proven undelivered, so no later idle edge may report the run
// finished and hand on_complete a session whose prompt never landed (#5278).
// Caller holds i.mu. The adoption baseline is pinned here for the same reason
// the other marker-clearing edges pin it: this is one of them.
func (i *Instance) endTaskRunFailedLocked() {
	i.taskRunActive = false
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
	i.taskRunFailed = true
	i.touchLocked()
	i.captureAdoptionBaselineLocked()
}

// rejectTransitionLocked builds the rejection error, fires the illegal-transition
// hook (panic in test), and returns the soft error. Caller holds i.mu; nothing
// is mutated, so a rejected transition leaves the state untouched.
func (i *Instance) rejectTransitionLocked(ev TransitionEvent, from stateAxes, why string) error {
	msg := fmt.Sprintf("illegal session transition %s from (liveness=%s, op=%s): %s",
		ev.kind, livenessLabel(from.liveness), opLabel(from.op), why)
	if onIllegalTransition != nil {
		onIllegalTransition(msg)
	}
	return errors.New(msg)
}

// livenessLabel / opLabel render the axes for transition error messages without
// adding String() methods to the axis types (which would change %v formatting
// elsewhere, e.g. in existing status logs).
func livenessLabel(lv Liveness) string {
	switch lv {
	case LivenessUnset:
		return "Unset"
	case LiveRunning:
		return "Running"
	case LiveReady:
		return "Ready"
	case LiveLost:
		return "Lost"
	case LiveDead:
		return "Dead"
	case LiveArchived:
		return "Archived"
	case LiveLimitReached:
		return "LimitReached"
	}
	return fmt.Sprintf("Liveness(%d)", int(lv))
}

func opLabel(op InFlightOp) string {
	switch op {
	case OpNone:
		return "None"
	case OpCreating:
		return "Creating"
	case OpKilling:
		return "Killing"
	case OpArchiving:
		return "Archiving"
	case OpRestoring:
		return "Restoring"
	case OpReplacing:
		return "Replacing"
	case OpRespawning:
		return "Respawning"
	}
	return fmt.Sprintf("InFlightOp(%d)", int(op))
}
